package main

import (
	"errors"
	"log"
	"math"
	"net/http"
	"os"
	"path/filepath"
	"sync"
	"time"
	"unsafe"

	lt "github.com/scakemyer/libtorrent-go"
)

const (
	piecesRefreshDuration = 500 * time.Millisecond
)

type TorrentFS struct {
	handle lt.TorrentHandle
	Dir    http.Dir
}

type TorrentFile struct {
	*os.File
	tfs               *TorrentFS
	torrentInfo       lt.TorrentInfo
	fileEntryIdx      int
	pieceLength       int
	fileOffset        int64
	fileSize          int64
	piecesMx          sync.RWMutex
	pieces            Bitfield
	piecesLastUpdated time.Time
	lastStatus        lt.TorrentStatus
	closed            bool
	savePath          string
}

func NewTorrentFS(handle lt.TorrentHandle, path string) *TorrentFS {
	tfs := TorrentFS{
		handle: handle,
		Dir:    http.Dir(path),
	}
	return &tfs
}

func (tfs *TorrentFS) Open(name string) (http.File, error) {
	file, err := os.Open(filepath.Join(string(tfs.Dir), name))
	if err != nil {
		return nil, err
	}
	// make sure we don't open a file that's locked, as it can happen
	// on BSD systems (darwin included)
	if err := unlockFile(file); err != nil {
		log.Printf("unable to unlock file because: %s", err)
	}

	if !tfs.handle.IsValid() {
		return nil, errors.New("file is not found")
	}

	torrentInfo := tfs.handle.TorrentFile()
	numFiles := torrentInfo.NumFiles()
	files := torrentInfo.Files()

	for j := 0; j < numFiles; j++ {
		path := files.FilePath(j)
		if name[1:] == path {
			return NewTorrentFile(file, tfs, torrentInfo, j, files.FileOffset(j), files.FileSize(j), path)
		}
	}
	defer lt.DeleteTorrentInfo(torrentInfo)

	return file, err
}

func NewTorrentFile(file *os.File, tfs *TorrentFS, torrentInfo lt.TorrentInfo, fileEntryIdx int, offset int64, size int64, path string) (*TorrentFile, error) {
	tf := &TorrentFile{
		File:         file,
		tfs:          tfs,
		torrentInfo:  torrentInfo,
		fileEntryIdx: fileEntryIdx,
		pieceLength:  torrentInfo.PieceLength(),
		fileOffset:   offset,
		fileSize:     size,
	}
	tf.log("opening file %s", path)
	return tf, nil
}

func (tf *TorrentFile) log(message string, v ...interface{}) {
	args := append([]interface{}{tf.fileEntryIdx}, v...)
	log.Printf("[%d] "+message+"\n", args...)
}

func (tf *TorrentFile) updatePieces() error {
	tf.piecesMx.Lock()
	defer tf.piecesMx.Unlock()

	if time.Now().After(tf.piecesLastUpdated.Add(piecesRefreshDuration)) {
		// need to keep a reference to the status or else the pieces bitfield
		// is at risk of being collected
		tf.lastStatus = tf.tfs.handle.Status(uint(lt.TorrentHandleQueryPieces))
		if tf.lastStatus.GetState() > lt.TorrentStatusSeeding {
			return errors.New("torrent file has invalid state")
		}
		piecesBits := tf.lastStatus.GetPieces()
		piecesBitsSize := piecesBits.Size()
		piecesSliceSize := piecesBitsSize / 8
		if piecesBitsSize%8 > 0 {
			// Add +1 to round up the bitfield
			piecesSliceSize++
		}
		data := (*[100000000]byte)(unsafe.Pointer(piecesBits.Bytes()))[:piecesSliceSize]
		tf.pieces = Bitfield(data)
		tf.piecesLastUpdated = time.Now()
	}
	return nil
}

func (tf *TorrentFile) getPieces() (int, int) {
	startPiece, _ := tf.pieceFromOffset(1)
	endPiece, _ := tf.pieceFromOffset(tf.fileSize - 1)
	return startPiece, endPiece
}

func (tf *TorrentFile) pieceFromOffset(offset int64) (int, int) {
	piece := (tf.fileOffset + offset) / int64(tf.pieceLength)
	pieceOffset := (tf.fileOffset + offset) % int64(tf.pieceLength)
	return int(piece), int(pieceOffset)
}

func (tf *TorrentFile) hasPiece(idx int) bool {
	if err := tf.updatePieces(); err != nil {
		return false
	}
	tf.piecesMx.RLock()
	defer tf.piecesMx.RUnlock()
	return tf.pieces.GetBit(idx)
}

func (tf *TorrentFile) waitForPiece(piece int) error {
	if tf.hasPiece(piece) {
		return nil
	}

	tf.log("waiting for piece %d", piece)
	tf.tfs.handle.SetPieceDeadline(piece, 100)

	ticker := time.Tick(piecesRefreshDuration)
	for tf.hasPiece(piece) == false {
		select {
		case <-ticker:
			if tf.tfs.handle.PiecePriority(piece).(int) == 0 || tf.closed {
				return errors.New("file was closed")
			}
			continue
		}
	}
	_, endPiece := tf.getPieces()
	if piece < endPiece && !tf.hasPiece(piece+1) {
		tf.tfs.handle.SetPieceDeadline(piece+1, 500)
	}
	return nil
}

func (tf *TorrentFile) Read(data []byte) (int, error) {
	currentOffset, err := tf.File.Seek(0, os.SEEK_CUR)
	if err != nil {
		return 0, err
	}

	piece, _ := tf.pieceFromOffset(currentOffset + int64(len(data)))
	if err := tf.waitForPiece(piece); err != nil {
		return 0, err
	}

	return tf.File.Read(data)
}

func (tf *TorrentFile) Seek(offset int64, whence int) (int64, error) {
	seekingOffset := offset

	switch whence {
	case os.SEEK_CUR:
		currentOffset, err := tf.File.Seek(0, os.SEEK_CUR)
		if err != nil {
			return currentOffset, err
		}
		seekingOffset += currentOffset
		break
	case os.SEEK_END:
		seekingOffset = tf.fileSize - offset
		break
	}

	tf.log("seeking at %d/%d", seekingOffset, tf.fileSize)
	piece, _ := tf.pieceFromOffset(seekingOffset)

	if tf.hasPiece(piece) == false {
		tf.log("we don't have piece %d, setting piece priorities", piece)
		piecesPriorities := lt.NewStdVectorInt()
		defer lt.DeleteStdVectorInt(piecesPriorities)

		curPiece := 0
		numPieces := tf.torrentInfo.NumPieces()
		startPiece, endPiece := tf.getPieces()
		buffPieces := int(math.Ceil(float64(endPiece-startPiece) * startBufferPercent))
		if buffPieces == 0 {
			buffPieces = 1
		}
		if piece+buffPieces > endPiece {
			buffPieces = endPiece - piece
		}
		for _ = 0; curPiece < piece; curPiece++ {
			piecesPriorities.Add(0)
		}
		for _ = 0; curPiece < piece+buffPieces; curPiece++ { //highest priority for buffer
			piecesPriorities.Add(7)
			tf.tfs.handle.SetPieceDeadline(curPiece, 0, 0)
		}
		for _ = 0; curPiece <= endPiece; curPiece++ { // to the end of a file
			piecesPriorities.Add(1)
		}
		for _ = 0; curPiece < numPieces; curPiece++ {
			piecesPriorities.Add(0)
		}
		tf.tfs.handle.PrioritizePieces(piecesPriorities)
	}
	return tf.File.Seek(offset, whence)
}

func (tf *TorrentFile) Close() (err error) {
	err = nil
	if tf.closed {
		return
	}
	files := torrentInfo.Files()
	tf.log("closing %s...", files.FilePath(fileEntryIdx))
	tf.closed = true
	if tf.File != nil {
		err = tf.File.Close()
	}
	return
}

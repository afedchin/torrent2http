package main

import (
	"encoding/json"
	"flag"
	"fmt"
	"io/ioutil"
	"log"
	"math"
	"math/rand"
	"net"
	"net/http"
	"net/url"
	"os"
	"os/signal"
	"path"
	"path/filepath"
	"runtime"
	"strconv"
	"strings"
	"sync"
	"syscall"
	"time"

	lt "github.com/scakemyer/libtorrent-go"
)

var dhtBootstrapNodes = []string{
	"router.bittorrent.com",
	"router.utorrent.com",
	"dht.transmissionbt.com",
	"dht.aelitis.com", // Vuze
}

var defaultTrackers = []string{
	"udp://tracker.opentrackr.org:1337/announce",
	"udp://tracker.coppersurfer.tk:6969/announce",
	"udp://tracker.leechers-paradise.org:6969/announce",
	"udp://tracker.openbittorrent.com:80/announce",
	"udp://explodie.org:6969",
}

type FileStatusInfo struct {
	Name     string  `json:"name"`
	SavePath string  `json:"save_path"`
	URL      string  `json:"url"`
	Size     int64   `json:"size"`
	Buffer   float64 `json:"buffer"`
}

type LsInfo struct {
	Files []FileStatusInfo `json:"file"`
}

type SessionStatus struct {
	Name          string  `json:"name"`
	State         int     `json:"state"`
	StateStr      string  `json:"state_str"`
	Error         string  `json:"error"`
	Progress      float32 `json:"progress"`
	DownloadRate  float32 `json:"download_rate"`
	UploadRate    float32 `json:"upload_rate"`
	TotalDownload int64   `json:"total_download"`
	TotalUpload   int64   `json:"total_upload"`
	NumPeers      int     `json:"num_peers"`
	NumSeeds      int     `json:"num_seeds"`
	TotalSeeds    int     `json:"total_seeds"`
	TotalPeers    int     `json:"total_peers"`
}

type Config struct {
	uri                 string
	bindAddress         string
	fileIndex           int
	maxUploadRate       int
	maxDownloadRate     int
	connectionsLimit    int
	downloadPath        string
	resumeFile          string
	stateFile           string
	userAgent           string
	keepComplete        bool
	keepIncomplete      bool
	keepFiles           bool
	encryption          int
	noSparseFile        bool
	idleTimeout         int
	peerConnectTimeout  int
	requestTimeout      int
	torrentConnectBoost int
	connectionSpeed     int
	listenPort          int
	minReconnectTime    int
	maxFailCount        int
	randomPort          bool
	debugAlerts         bool
	enableScrape        bool
	enableDHT           bool
	enableLSD           bool
	enableUPNP          bool
	enableNATPMP        bool
	enableUTP           bool
	enableTCP           bool
	exitOnFinish        bool
	dhtRouters          string
	trackers            string
	buffer              float64
}

const (
	startBufferPercent = 0.005
	endBufferSize      = 10 * 1024 * 1024 // 10m
	minCandidateSize   = 100 * 1024 * 1024
	defaultDHTPort     = 6881
)

var (
	config                   Config
	session                  lt.Session
	torrentHandle            lt.TorrentHandle
	torrentInfo              lt.TorrentInfo
	torrentFS                *TorrentFS
	forceShutdown            chan bool
	httpListener             net.Listener
	chosenFile               lt.FileEntry
	chosenIndex              int
	bufferPiecesProgressLock sync.RWMutex
	bufferPiecesProgress     = make(map[int]float64)
)

const (
	STATE_QUEUED_FOR_CHECKING = iota
	STATE_CHECKING_FILES
	STATE_DOWNLOADING_METADATA
	STATE_DOWNLOADING
	STATE_FINISHED
	STATE_SEEDING
	STATE_ALLOCATING
	STATE_CHECKING_RESUME_DATA
)

var stateStrings = map[int]string{
	STATE_QUEUED_FOR_CHECKING:  "queued_for_checking",
	STATE_CHECKING_FILES:       "checking_files",
	STATE_DOWNLOADING_METADATA: "downloading_metadata",
	STATE_DOWNLOADING:          "downloading",
	STATE_FINISHED:             "finished",
	STATE_SEEDING:              "seeding",
	STATE_ALLOCATING:           "allocating",
	STATE_CHECKING_RESUME_DATA: "checking_resume_data",
}

func statusHandler(w http.ResponseWriter, _ *http.Request) {
	w.Header().Set("Content-Type", "application/json")

	var status SessionStatus
	if torrentHandle == nil {
		status = SessionStatus{State: -1}
	} else {
		tstatus := torrentHandle.Status()
		status = SessionStatus{
			Name:          tstatus.GetName(),
			State:         int(tstatus.GetState()),
			StateStr:      stateStrings[int(tstatus.GetState())],
			Error:         tstatus.GetError(),
			Progress:      tstatus.GetProgress(),
			TotalDownload: tstatus.GetTotalDownload(),
			TotalUpload:   tstatus.GetTotalUpload(),
			DownloadRate:  float32(tstatus.GetDownloadRate()) / 1024,
			UploadRate:    float32(tstatus.GetUploadRate()) / 1024,
			NumPeers:      tstatus.GetNumPeers(),
			TotalPeers:    tstatus.GetNumIncomplete(),
			NumSeeds:      tstatus.GetNumSeeds(),
			TotalSeeds:    tstatus.GetNumComplete()}
	}

	output, _ := json.Marshal(status)
	w.Write(output)
}

func lsHandler(w http.ResponseWriter, _ *http.Request) {
	w.Header().Set("Content-Type", "application/json")

	retFiles := LsInfo{}

	if torrentHandle.IsValid() && torrentInfo != nil {
		if chosenIndex >= 0 && chosenIndex < torrentInfo.NumFiles() {
			state := torrentHandle.Status().GetState()
			bufferProgress := float64(0)
			if state != STATE_CHECKING_FILES && state != STATE_QUEUED_FOR_CHECKING {
				bufferPiecesProgressLock.Lock()
				lenght := len(bufferPiecesProgress)
				if lenght > 0 {
					totalProgress := float64(0)
					piecesProgress(bufferPiecesProgress)
					for _, v := range bufferPiecesProgress {
						totalProgress += v
					}
					bufferProgress = totalProgress / float64(lenght)
				}
				bufferPiecesProgressLock.Unlock()
			}

			fileEntry := torrentInfo.FileAt(chosenIndex)
			path, _ := filepath.Abs(path.Join(config.downloadPath, fileEntry.GetPath()))

			url := url.URL{
				Host:   config.bindAddress,
				Path:   "/files/" + fileEntry.GetPath(),
				Scheme: "http",
			}
			fsi := FileStatusInfo{
				Buffer:   bufferProgress,
				Name:     fileEntry.GetPath(),
				Size:     fileEntry.GetSize(),
				SavePath: path,
				URL:      url.String(),
			}
			retFiles.Files = append(retFiles.Files, fsi)
		}
	}

	output, _ := json.Marshal(retFiles)
	w.Write(output)
}

func filesToRemove() []string {
	var files []string
	if torrentInfo != nil {
		progresses := lt.NewStdVectorSizeType()
		defer lt.DeleteStdVectorSizeType(progresses)

		torrentHandle.FileProgress(progresses, int(lt.TorrentHandlePieceGranularity))
		numFiles := torrentInfo.NumFiles()
		for i := 0; i < numFiles; i++ {
			fileEntry := torrentInfo.FileAt(i)
			downloaded := progresses.Get(i)
			size := fileEntry.GetSize()
			completed := downloaded == size

			if (!config.keepComplete || !completed) && (!config.keepIncomplete || completed) {
				savePath, _ := filepath.Abs(path.Join(config.downloadPath, fileEntry.GetPath()))
				if _, err := os.Stat(savePath); !os.IsNotExist(err) {
					files = append(files, savePath)
				}
			}
		}
	}
	return files
}

func trimPathSeparator(path string) string {
	last := len(path) - 1
	if last > 0 && os.IsPathSeparator(path[last]) {
		path = path[:last]
	}
	return path
}

func removeFiles(files []string) {
	for _, file := range files {
		if err := os.Remove(file); err != nil {
			log.Println(err)
		} else {
			// Remove empty folders as well
			path := filepath.Dir(file)
			savePath, _ := filepath.Abs(config.downloadPath)
			savePath = trimPathSeparator(savePath)
			for path != savePath {
				os.Remove(path)
				path = trimPathSeparator(filepath.Dir(path))
			}
		}
	}
}

func waitForAlert(name string, timeout time.Duration) lt.Alert {
	start := time.Now()
	for {
		for {
			alert := session.WaitForAlert(lt.Milliseconds(100))
			if time.Now().Sub(start) > timeout {
				return nil
			}
			if alert.Swigcptr() != 0 {
				alert = popAlert(false)
				if alert.What() == name {
					return alert
				}
			}
		}
	}
}

func removeTorrent() {
	var flag int
	var files []string

	state := torrentHandle.Status().GetState()
	if state != STATE_CHECKING_FILES && state != STATE_QUEUED_FOR_CHECKING && !config.keepFiles {
		if !config.keepComplete && !config.keepIncomplete {
			flag = int(lt.SessionDeleteFiles)
		} else {
			files = filesToRemove()
		}
	}
	log.Println("removing the torrent")
	session.RemoveTorrent(torrentHandle, flag)
	if flag != 0 || len(files) > 0 {
		log.Println("waiting for files to be removed")
		waitForAlert("cache_flushed_alert", 15*time.Second)
		removeFiles(files)
	}
}

func saveResumeData(async bool) bool {
	if !torrentHandle.Status().GetNeedSaveResume() || config.resumeFile == "" {
		return false
	}
	torrentHandle.SaveResumeData(3)
	if !async {
		alert := waitForAlert("save_resume_data_alert", 5*time.Second)
		if alert == nil {
			return false
		}
		processSaveResumeDataAlert(alert)
	}
	return true
}

func saveSessionState() {
	if config.stateFile == "" {
		return
	}
	entry := lt.NewEntry()
	session.SaveState(entry)
	data := lt.Bencode(entry)
	log.Printf("saving session state to: %s", config.stateFile)
	err := ioutil.WriteFile(config.stateFile, []byte(data), 0644)
	if err != nil {
		log.Println(err)
	}
}

func shutdown() {
	log.Println("stopping torrent2http...")
	if session != nil {
		session.Pause()
		waitForAlert("torrent_paused_alert", 10*time.Second)
		if torrentHandle != nil {
			saveResumeData(false)
			saveSessionState()
			removeTorrent()
		}
		log.Println("aborting the session")
		lt.DeleteSession(session)
	}
	log.Println("bye bye")
	os.Exit(0)
}

func parseFlags() {
	config = Config{}
	flag.StringVar(&config.uri, "uri", "", "Magnet URI or .torrent file URL")
	flag.StringVar(&config.bindAddress, "bind", "localhost:5001", "Bind address of torrent2http")
	flag.StringVar(&config.downloadPath, "dl-path", ".", "Download path")
	flag.IntVar(&config.idleTimeout, "max-idle", -1, "Automatically shutdown if no connection are active after a timeout (seconds)")
	flag.IntVar(&config.fileIndex, "file-index", -1, "Start downloading file with specified index immediately (or start in paused state otherwise)")
	flag.BoolVar(&config.keepComplete, "keep-complete", false, "Keep complete files after exiting")
	flag.BoolVar(&config.keepIncomplete, "keep-incomplete", false, "Keep incomplete files after exiting")
	flag.BoolVar(&config.keepFiles, "keep-files", false, "Keep all files after exiting (incl. -keep-complete and -keep-incomplete)")
	flag.BoolVar(&config.debugAlerts, "debug-alerts", false, "Show debug alert notifications")
	flag.BoolVar(&config.exitOnFinish, "exit-on-finish", false, "Exit when download finished")

	flag.StringVar(&config.resumeFile, "resume-file", "", "Use fast resume file")
	flag.StringVar(&config.stateFile, "state-file", "", "Use file for saving/restoring session state")
	flag.StringVar(&config.userAgent, "user-agent", UserAgent(), "Set an user agent")
	flag.StringVar(&config.dhtRouters, "dht-routers", "", "Additional DHT routers (comma-separated host:port pairs)")
	flag.StringVar(&config.trackers, "trackers", "", "Additional trackers (comma-separated URLs)")
	flag.IntVar(&config.listenPort, "listen-port", 6881, "Use specified port for incoming connections")
	flag.IntVar(&config.torrentConnectBoost, "torrent-connect-boost", 50, "The number of peers to try to connect to immediately when the first tracker response is received for a torrent")
	flag.IntVar(&config.connectionSpeed, "connection-speed", 500, "The number of peer connection attempts that are made per second")
	flag.IntVar(&config.peerConnectTimeout, "peer-connect-timeout", 2, "The number of seconds to wait after a connection attempt is initiated to a peer")
	flag.IntVar(&config.requestTimeout, "request-timeout", 2, "The number of seconds until the current front piece request will time out")
	flag.IntVar(&config.maxDownloadRate, "dl-rate", -1, "Max download rate (kB/s)")
	flag.IntVar(&config.maxUploadRate, "ul-rate", -1, "Max upload rate (kB/s)")
	flag.IntVar(&config.connectionsLimit, "connections-limit", 200, "Set a global limit on the number of connections opened")
	flag.IntVar(&config.encryption, "encryption", 1, "Encryption: 0=forced 1=enabled (default) 2=disabled")
	flag.IntVar(&config.minReconnectTime, "min-reconnect-time", 60, "The time to wait between peer connection attempts. If the peer fails, the time is multiplied by fail counter")
	flag.IntVar(&config.maxFailCount, "max-failcount", 3, "The maximum times we try to connect to a peer before stop connecting again")
	flag.BoolVar(&config.noSparseFile, "no-sparse", false, "Do not use sparse file allocation")
	flag.BoolVar(&config.randomPort, "random-port", false, "Use random listen port (49152-65535)")
	flag.BoolVar(&config.enableScrape, "enable-scrape", false, "Enable sending scrape request to tracker (updates total peers/seeds count)")
	flag.BoolVar(&config.enableDHT, "enable-dht", true, "Enable DHT (Distributed Hash Table)")
	flag.BoolVar(&config.enableLSD, "enable-lsd", true, "Enable LSD (Local Service Discovery)")
	flag.BoolVar(&config.enableUPNP, "enable-upnp", true, "Enable UPnP (UPnP port-mapping)")
	flag.BoolVar(&config.enableNATPMP, "enable-natpmp", true, "Enable NATPMP (NAT port-mapping)")
	flag.BoolVar(&config.enableUTP, "enable-utp", true, "Enable uTP protocol")
	flag.BoolVar(&config.enableTCP, "enable-tcp", true, "Enable TCP protocol")
	flag.Float64Var(&config.buffer, "buffer", startBufferPercent, "Buffer percentage from start of file")
	flag.Parse()

	if config.uri == "" {
		flag.Usage()
		os.Exit(1)
	}
	if config.resumeFile != "" && !config.keepFiles {
		fmt.Println("Usage of option -resume-file is allowed only along with -keep-files")
		os.Exit(1)
	}
}

func NewConnectionCounterHandler(connTrackChannel chan int, handler http.Handler) http.Handler {
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		connTrackChannel <- 1
		handler.ServeHTTP(w, r)
		connTrackChannel <- -1
	})
}

func inactiveAutoShutdown(connTrackChannel chan int) {
	activeConnections := 0

	for {
		if activeConnections == 0 {
			select {
			case inc := <-connTrackChannel:
				activeConnections += inc
			case <-time.After(time.Duration(config.idleTimeout) * time.Second):
				forceShutdown <- true
			}
		} else {
			activeConnections += <-connTrackChannel
		}
	}
}

func startHTTP() {
	log.Println("starting HTTP Server...")

	mux := http.NewServeMux()
	mux.HandleFunc("/status", statusHandler)
	mux.HandleFunc("/ls", lsHandler)
	mux.HandleFunc("/shutdown", func(w http.ResponseWriter, _ *http.Request) {
		fmt.Fprintf(w, "OK")
		forceShutdown <- true
	})
	mux.Handle("/files/", http.StripPrefix("/files/", http.FileServer(torrentFS)))

	handler := http.Handler(mux)
	if config.idleTimeout > 0 {
		connTrackChannel := make(chan int, 10)
		handler = NewConnectionCounterHandler(connTrackChannel, mux)
		go inactiveAutoShutdown(connTrackChannel)
	}

	log.Printf("listening HTTP on %s\n", config.bindAddress)
	s := &http.Server{
		Addr:    config.bindAddress,
		Handler: handler,
	}

	var e error
	if httpListener, e = net.Listen("tcp", config.bindAddress); e != nil {
		log.Fatal(e)
	}
	go s.Serve(httpListener)
}

func popAlert(logAlert bool) lt.Alert {
	alert := session.PopAlert()
	if alert.Swigcptr() == 0 {
		return nil
	}
	if logAlert {
		str := ""
		switch alert.What() {
		case "tracker_error_alert":
			str = lt.SwigcptrTrackerErrorAlert(alert.Swigcptr()).GetMsg()
			break
		case "tracker_warning_alert":
			str = lt.SwigcptrTrackerWarningAlert(alert.Swigcptr()).GetMsg()
			break
		case "scrape_failed_alert":
			str = lt.SwigcptrScrapeFailedAlert(alert.Swigcptr()).GetMsg()
			break
		case "url_seed_alert":
			str = lt.SwigcptrUrlSeedAlert(alert.Swigcptr()).GetMsg()
			break
		}
		if str != "" {
			log.Printf("(%s) %s: %s", alert.What(), alert.Message(), str)
		} else {
			log.Printf("(%s) %s", alert.What(), alert.Message())
		}
	}
	return alert
}

func processSaveResumeDataAlert(alert lt.Alert) {
	saveResumeDataAlert := lt.SwigcptrSaveResumeDataAlert(alert.Swigcptr())
	log.Printf("saving resume data to: %s", config.resumeFile)
	data := lt.Bencode(saveResumeDataAlert.ResumeData())
	err := ioutil.WriteFile(config.resumeFile, []byte(data), 0644)
	if err != nil {
		log.Println(err)
	}
}

func consumeAlerts() {
	for {
		var alert lt.Alert
		if alert = popAlert(true); alert == nil {
			break
		}
		switch alert.What() {
		case "save_resume_data_alert":
			processSaveResumeDataAlert(alert)
			break
		case "metadata_received_alert":
			onMetadataReceived()
			break
		}
	}
}

func buildTorrentParams(uri string) lt.AddTorrentParams {
	fileUri, err := url.Parse(uri)
	torrentParams := lt.NewAddTorrentParams()
	error := lt.NewErrorCode()
	if err != nil {
		log.Fatal(err)
	}
	if fileUri.Scheme == "file" {
		uriPath := fileUri.Path
		if uriPath != "" && runtime.GOOS == "windows" && os.IsPathSeparator(uriPath[0]) {
			uriPath = uriPath[1:]
		}
		absPath, err := filepath.Abs(uriPath)
		if err != nil {
			log.Fatalf(err.Error())
		}
		log.Printf("opening local file: %s", absPath)
		if _, err := os.Stat(absPath); err != nil {
			log.Fatalf(err.Error())
		}
		torrentInfo := lt.NewTorrentInfo(absPath, error)
		if error.Value() != 0 {
			log.Fatalln(error.Message())
		}
		torrentParams.SetTorrentInfo(torrentInfo)
	} else {
		log.Printf("will fetch: %s", uri)
		torrentParams.SetUrl(uri)
	}

	log.Printf("setting save path: %s", config.downloadPath)
	torrentParams.SetSavePath(config.downloadPath)

	if _, err := os.Stat(config.resumeFile); !os.IsNotExist(err) {
		log.Printf("loading resume file: %s", config.resumeFile)
		bytes, err := ioutil.ReadFile(config.resumeFile)
		if err != nil {
			log.Println(err)
		} else {
			resumeData := lt.NewStdVectorChar()
			count := 0
			for _, byte := range bytes {
				resumeData.PushBack(byte)
				count++
			}
			torrentParams.SetResumeData(resumeData)
		}
	}

	if config.noSparseFile {
		log.Println("disabling sparse file support...")
		torrentParams.SetStorageMode(lt.StorageModeAllocate)
	}

	return torrentParams
}

func startServices() {
	if config.enableDHT {
		log.Println("starting DHT...")
		if config.dhtRouters != "" {
			routers := strings.Split(config.dhtRouters, ",")
			for _, router := range routers {
				router = strings.TrimSpace(router)
				if len(router) != 0 {
					var err error
					hostPort := strings.SplitN(router, ":", 2)
					host := strings.TrimSpace(hostPort[0])
					port := defaultDHTPort
					if len(hostPort) > 1 {
						port, err = strconv.Atoi(strings.TrimSpace(hostPort[1]))
						if err != nil {
							log.Fatalln(err)
						}
					}
					session.AddDhtRouter(lt.NewStdPairStringInt(host, port))
					log.Printf("added DHT router: %s:%d", host, port)
				}
			}
		} else {
			for _, node := range dhtBootstrapNodes {
				pair := lt.NewStdPairStringInt(node, defaultDHTPort)
				defer lt.DeleteStdPairStringInt(pair)
				session.AddDhtRouter(pair)
				log.Printf("added DHT router: %s:%d", node, defaultDHTPort)
			}
		}
		session.StartDht()
	}
	if config.enableLSD {
		log.Println("starting LSD...")
		session.StartLsd()
	}
	if config.enableUPNP {
		log.Println("starting UPNP...")
		session.StartUpnp()
	}
	if config.enableNATPMP {
		log.Println("starting NATPMP...")
		session.StartNatpmp()
	}
}

func startSession() {
	log.Println("starting session...")

	session = lt.NewSession()
	alertMask := uint(lt.AlertErrorNotification) | uint(lt.AlertStorageNotification) |
		uint(lt.AlertTrackerNotification) | uint(lt.AlertStatusNotification)
	if config.debugAlerts {
		alertMask |= uint(lt.AlertDebugNotification)
	}
	session.SetAlertMask(alertMask)

	settings := session.Settings()
	settings.SetRequestTimeout(config.requestTimeout)
	settings.SetPeerConnectTimeout(config.peerConnectTimeout)
	settings.SetStrictEndGameMode(true)
	settings.SetAnnounceToAllTrackers(true)
	settings.SetAnnounceToAllTiers(true)
	settings.SetTorrentConnectBoost(config.torrentConnectBoost)
	settings.SetRateLimitIpOverhead(true)
	settings.SetAnnounceDoubleNat(true)
	settings.SetPrioritizePartialPieces(false)
	settings.SetFreeTorrentHashes(true)
	settings.SetUseParoleMode(true)

	// Make sure the disk cache is not swapped out (useful for slower devices)
	settings.SetLockDiskCache(true)
	settings.SetDiskCacheAlgorithm(lt.SessionSettingsLargestContiguous)

	// Prioritize people starting downloads
	settings.SetSeedChokingAlgorithm(int(lt.SessionSettingsFastestUpload))

	// copied from qBitorrent at
	// https://github.com/qbittorrent/qBittorrent/blob/master/src/qtlibtorrent/qbtsession.cpp
	settings.SetUpnpIgnoreNonrouters(true)
	settings.SetLazyBitfields(true)
	settings.SetStopTrackerTimeout(1)
	settings.SetAutoScrapeInterval(1200)   // 20 minutes
	settings.SetAutoScrapeMinInterval(900) // 15 minutes
	settings.SetIgnoreLimitsOnLocalNetwork(true)
	settings.SetRateLimitUtp(true)
	settings.SetMixedModeAlgorithm(int(lt.SessionSettingsPreferTcp))

	settings.SetConnectionSpeed(config.connectionSpeed)
	settings.SetMinReconnectTime(config.minReconnectTime)
	settings.SetMaxFailcount(config.maxFailCount)

	setPlatformSpecificSettings(settings)
	session.SetSettings(settings)
	session.AddExtensions()

	if config.stateFile != "" {
		log.Printf("loading session state from %s", config.stateFile)
		bytes, err := ioutil.ReadFile(config.stateFile)
		if err != nil {
			log.Println(err)
		} else {
			str := string(bytes)
			entry := lt.NewLazyEntry()
			error := lt.LazyBdecode(str, entry).(lt.ErrorCode)
			if error.Value() != 0 {
				log.Println(error.Message())
			} else {
				session.LoadState(entry)
			}
		}
	}

	err := lt.NewErrorCode()
	rand.Seed(time.Now().UnixNano())
	portLower := config.listenPort
	if config.randomPort {
		portLower = rand.Intn(16374) + 49152
	}
	portUpper := portLower + 10
	session.ListenOn(lt.NewStdPairIntInt(portLower, portUpper), err)
	if err.Value() != 0 {
		log.Fatalln(err.Message())
	}

	settings = session.Settings()
	if config.userAgent != "" {
		settings.SetUserAgent(config.userAgent)
	}
	if config.connectionsLimit >= 0 {
		settings.SetConnectionsLimit(config.connectionsLimit)
	}
	if config.maxDownloadRate >= 0 {
		settings.SetDownloadRateLimit(config.maxDownloadRate * 1024)
	}
	if config.maxUploadRate >= 0 {
		settings.SetUploadRateLimit(config.maxUploadRate * 1024)
	}
	settings.SetEnableIncomingTcp(config.enableTCP)
	settings.SetEnableOutgoingTcp(config.enableTCP)
	settings.SetEnableIncomingUtp(config.enableUTP)
	settings.SetEnableOutgoingUtp(config.enableUTP)
	setPlatformSpecificSettings(settings)
	session.SetSettings(settings)

	log.Println("setting encryption settings")
	encryptionSettings := lt.NewPeSettings()
	encryptionSettings.SetOutEncPolicy(byte(lt.LibtorrentPe_settingsEnc_policy(config.encryption)))
	encryptionSettings.SetInEncPolicy(byte(lt.LibtorrentPe_settingsEnc_policy(config.encryption)))
	encryptionSettings.SetAllowedEncLevel(byte(lt.PeSettingsBoth))
	encryptionSettings.SetPreferRc4(true)
	session.SetPeSettings(encryptionSettings)
}

func chooseFile() (lt.FileEntry, int) {
	var biggestFile lt.FileEntry
	biggestFileIndex := int(0)
	maxSize := int64(0)
	numFiles := torrentInfo.NumFiles()
	candidateFiles := make(map[int]bool)

	for i := 0; i < numFiles; i++ {
		fe := torrentInfo.FileAt(i)
		size := fe.GetSize()
		if size > maxSize {
			maxSize = size
			biggestFile = fe
			biggestFileIndex = i
		}
		if size > minCandidateSize {
			candidateFiles[i] = true
		}
	}

	log.Printf("there are %d candidate file(s)", len(candidateFiles))

	if config.fileIndex >= 0 {
		if _, ok := candidateFiles[config.fileIndex]; ok {
			log.Printf("selecting file at selecting requested file (%d)", config.fileIndex)
			return torrentInfo.FileAt(config.fileIndex), config.fileIndex
		}
		log.Print("unable to select requested file")
	}

	log.Printf("selecting most biggest file (%d with size %dkB)", biggestFileIndex, maxSize/1024)
	return biggestFile, biggestFileIndex
}

func pieceFromOffset(offset int64) (int, int64) {
	pieceLength := int64(torrentInfo.PieceLength())
	piece := int(offset / pieceLength)
	pieceOffset := offset % pieceLength
	return piece, pieceOffset
}

func getFilePiecesAndOffset(fe lt.FileEntry) (int, int, int64) {
	startPiece, offset := pieceFromOffset(fe.GetOffset())
	endPiece, _ := pieceFromOffset(fe.GetOffset() + fe.GetSize())
	return startPiece, endPiece, offset
}

func addTorrent(torrentParams lt.AddTorrentParams) {
	log.Println("adding torrent")
	error := lt.NewErrorCode()
	torrentHandle = session.AddTorrent(torrentParams, error)
	if error.Value() != 0 {
		log.Fatalln(error.Message())
	}

	log.Println("enabling sequential download")
	torrentHandle.SetSequentialDownload(true)

	trackers := defaultTrackers
	if config.trackers != "" {
		trackers = strings.Split(config.trackers, ",")
	}
	startTier := 256 - len(trackers)
	for n, tracker := range trackers {
		tracker = strings.TrimSpace(tracker)
		announceEntry := lt.NewAnnounceEntry(tracker)
		announceEntry.SetTier(byte(startTier + n))
		log.Printf("adding tracker: %s", tracker)
		torrentHandle.AddTracker(announceEntry)
	}

	if config.enableScrape {
		log.Println("sending scrape request to tracker")
		torrentHandle.ScrapeTracker()
	}

	log.Printf("downloading torrent: %s", torrentHandle.Status().GetName())
	torrentFS = NewTorrentFS(torrentHandle, config.downloadPath)

	if torrentHandle.Status().GetHasMetadata() {
		onMetadataReceived()
	}
}

func onMetadataReceived() {
	log.Printf("metadata received")

	torrentInfo = torrentHandle.TorrentFile()

	chosenFile, chosenIndex = chooseFile()

	log.Print("setting piece priorities")

	pieceLength := float64(torrentInfo.PieceLength())
	startPiece, endPiece, _ := getFilePiecesAndOffset(chosenFile)

	startLength := float64(endPiece-startPiece) * float64(pieceLength) * config.buffer
	startBufferPieces := int(math.Ceil(startLength / pieceLength))
	// Prefer a fixed size, since metadata are very rarely over endPiecesSize=10MB anyway.
	endBufferPieces := int(math.Ceil(float64(endBufferSize) / pieceLength))

	piecesPriorities := lt.NewStdVectorInt()
	defer lt.DeleteStdVectorInt(piecesPriorities)

	bufferPiecesProgressLock.Lock()
	defer bufferPiecesProgressLock.Unlock()

	// Properly set the pieces priority vector
	curPiece := 0
	for _ = 0; curPiece < startPiece; curPiece++ {
		piecesPriorities.PushBack(0)
	}
	for _ = 0; curPiece < startPiece+startBufferPieces; curPiece++ { // get this part
		piecesPriorities.PushBack(1)
		bufferPiecesProgress[curPiece] = 0
		torrentHandle.SetPieceDeadline(curPiece, 0, 0)
	}
	for _ = 0; curPiece < endPiece-endBufferPieces; curPiece++ {
		piecesPriorities.PushBack(1)
	}
	for _ = 0; curPiece <= endPiece; curPiece++ { // get this part
		piecesPriorities.PushBack(7)
		bufferPiecesProgress[curPiece] = 0
		torrentHandle.SetPieceDeadline(curPiece, 0, 0)
	}
	numPieces := torrentInfo.NumPieces()
	for _ = 0; curPiece < numPieces; curPiece++ {
		piecesPriorities.PushBack(0)
	}
	torrentHandle.PrioritizePieces(piecesPriorities)
}

func piecesProgress(pieces map[int]float64) {
	queue := lt.NewStdVectorPartialPieceInfo()
	defer lt.DeleteStdVectorPartialPieceInfo(queue)

	torrentHandle.GetDownloadQueue(queue)
	for piece := range pieces {
		if torrentHandle.HavePiece(piece) == true {
			pieces[piece] = 1.0
		}
	}
	queueSize := queue.Size()
	for i := 0; i < int(queueSize); i++ {
		ppi := queue.Get(i)
		pieceIndex := ppi.GetPieceIndex()
		if _, exists := pieces[pieceIndex]; exists {
			blocks := ppi.Blocks()
			totalBlocks := ppi.GetBlocksInPiece()
			totalBlockDownloaded := uint(0)
			totalBlockSize := uint(0)
			for j := 0; j < totalBlocks; j++ {
				block := blocks.Getitem(j)
				totalBlockDownloaded += block.GetBytesProgress()
				totalBlockSize += block.GetBlockSize()
			}
			pieces[pieceIndex] = float64(totalBlockDownloaded) / float64(totalBlockSize)
		}
	}
}

func loop() {
	forceShutdown = make(chan bool, 1)
	signalChan := make(chan os.Signal, 1)
	signal.Notify(signalChan, os.Interrupt, syscall.SIGTERM)
	saveResumeDataTicker := time.Tick(30 * time.Second)
	for {
		select {
		case <-forceShutdown:
			httpListener.Close()
			return
		case <-signalChan:
			forceShutdown <- true
		case <-time.After(500 * time.Millisecond):
			consumeAlerts()
			state := torrentHandle.Status().GetState()
			if config.exitOnFinish && (state == STATE_FINISHED || state == STATE_SEEDING) {
				forceShutdown <- true
			}
			if os.Getppid() == 1 {
				forceShutdown <- true
			}
		case <-saveResumeDataTicker:
			saveResumeData(true)
		}
	}
}

func main() {
	// Make sure we are properly multi-threaded, on a minimum of 2 threads
	// because we lock the main thread for lt.
	runtime.GOMAXPROCS(runtime.NumCPU())
	parseFlags()

	startSession()
	startServices()
	addTorrent(buildTorrentParams(config.uri))

	startHTTP()
	loop()
	shutdown()
}

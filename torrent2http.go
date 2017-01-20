package main

import (
	"encoding/json"
	"flag"
	"fmt"
	"io/ioutil"
	"log"
	"math"
	"net/http"
	"net/url"
	"os"
	"os/signal"
	"path"
	"path/filepath"
	"runtime"
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

const (
	ipToSDefault     = iota
	ipToSLowDelay    = 1 << iota
	ipToSReliability = 1 << iota
	ipToSThroughput  = 1 << iota
	ipToSLowCost     = 1 << iota
)

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
	packSettings             lt.SettingsPack
	session                  lt.Session
	torrentHandle            lt.TorrentHandle
	torrentInfo              lt.TorrentInfo
	torrentFS                *TorrentFS
	forceShutdown            chan bool
	fileEntryIdx             int
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

const (
	ERROR_NO_ERROR = iota
	ERROR_EXPECTED_DIGID
	ERROR_EXPECTED_COLON
	ERROR_UNEXPECTED_EOF
	ERROR_EXPECTED_VALUE
	ERROR_DEPTH_EXCEEDED
	ERROR_LIMIT_EXCEEDED
	ERROR_OVERFLOW
)

var errorStrings = map[int]string{
	ERROR_NO_ERROR:       "",
	ERROR_EXPECTED_DIGID: "expected digit in bencoded string",
	ERROR_EXPECTED_COLON: "expected colon in bencoded string",
	ERROR_UNEXPECTED_EOF: "unexpected end of file in bencoded string",
	ERROR_EXPECTED_VALUE: "expected value (list, dict, int or string) in bencoded string",
	ERROR_DEPTH_EXCEEDED: "bencoded recursion depth limit exceeded",
	ERROR_LIMIT_EXCEEDED: "bencoded item count limit exceeded",
	ERROR_OVERFLOW:       "integer overflow",
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
			Error:         errorStrings[tstatus.GetErrc().Value()],
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
		if fileEntryIdx >= 0 && fileEntryIdx < torrentInfo.NumFiles() {
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

			files := torrentInfo.Files()
			path, _ := filepath.Abs(path.Join(config.downloadPath, files.FilePath(fileEntryIdx)))

			url := url.URL{
				Host:   config.bindAddress,
				Path:   "/files/" + files.FilePath(fileEntryIdx),
				Scheme: "http",
			}
			fsi := FileStatusInfo{
				Buffer:   bufferProgress,
				Name:     files.FilePath(fileEntryIdx),
				Size:     files.FileSize(fileEntryIdx),
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
	var filesToRemove []string
	if torrentInfo != nil {
		progresses := lt.NewStdVectorSizeType()
		defer lt.DeleteStdVectorSizeType(progresses)

		torrentHandle.FileProgress(progresses, int(lt.TorrentHandlePieceGranularity))
		numFiles := torrentInfo.NumFiles()
		for i := 0; i < numFiles; i++ {
			files := torrentInfo.Files()
			downloaded := progresses.Get(i)
			size := files.FileSize(i)
			completed := downloaded == size

			if (!config.keepComplete || !completed) && (!config.keepIncomplete || completed) {
				savePath, _ := filepath.Abs(path.Join(config.downloadPath, files.FilePath(i)))
				if _, err := os.Stat(savePath); !os.IsNotExist(err) {
					filesToRemove = append(filesToRemove, savePath)
				}
			}
		}
	}
	return filesToRemove
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
	var retAlert lt.Alert
	for retAlert == nil {
		for retAlert == nil {
			alert := session.GetHandle().WaitForAlert(lt.Milliseconds(100))
			if time.Now().Sub(start) > timeout {
				return nil
			}
			if alert.Swigcptr() != 0 {
				var alerts lt.StdVectorAlerts
				alerts = session.GetHandle().PopAlerts()
				queueSize := alerts.Size()
				for i := 0; i < int(queueSize); i++ {
					alert := alerts.Get(i)
					if alert.What() == name {
						retAlert = alert
					}
					processAlert(alert)
				}
			}
		}
	}
	return retAlert
}

func removeTorrent() {
	var flag int
	var files []string

	state := torrentHandle.Status().GetState()
	if state != STATE_CHECKING_FILES && state != STATE_QUEUED_FOR_CHECKING && !config.keepFiles {
		if !config.keepComplete && !config.keepIncomplete {
			flag = int(lt.SessionHandleDeleteFiles)
		} else {
			files = filesToRemove()
		}
	}
	log.Println("removing the torrent")
	session.GetHandle().RemoveTorrent(torrentHandle, flag)
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
	session.GetHandle().SaveState(entry)
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
		session.GetHandle().Pause()
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
	flag.IntVar(&config.connectionsLimit, "connections-limit", 0, "Set a global limit on the number of connections opened")
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

func connectionCounterHandler(connTrackChannel chan int, handler http.Handler) http.Handler {
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

	http.HandleFunc("/status", statusHandler)
	http.HandleFunc("/ls", lsHandler)
	http.HandleFunc("/shutdown", func(w http.ResponseWriter, _ *http.Request) {
		fmt.Fprintf(w, "OK")
		forceShutdown <- true
	})
	http.Handle("/files/", http.StripPrefix("/files/", http.FileServer(torrentFS)))

	handler := http.Handler(http.DefaultServeMux)
	if config.idleTimeout > 0 {
		connTrackChannel := make(chan int, 10)
		handler = connectionCounterHandler(connTrackChannel, handler)
		go inactiveAutoShutdown(connTrackChannel)
	}

	log.Printf("listening HTTP on %s\n", config.bindAddress)
	if err := http.ListenAndServe(config.bindAddress, handler); err != nil {
		log.Fatal(err)
	}
}

func logAlert(alert lt.Alert) {
	str := ""
	switch alert.What() {
	case "tracker_error_alert":
		str = lt.SwigcptrTrackerErrorAlert(alert.Swigcptr()).ErrorMessage()
		break
	case "tracker_warning_alert":
		str = lt.SwigcptrTrackerWarningAlert(alert.Swigcptr()).WarningMessage()
		break
	case "scrape_failed_alert":
		str = lt.SwigcptrScrapeFailedAlert(alert.Swigcptr()).ErrorMessage()
		break
	case "url_seed_alert":
		str = lt.SwigcptrUrlSeedAlert(alert.Swigcptr()).ErrorMessage()
		break
	}
	if str != "" {
		log.Printf("(%s) %s: %s", alert.What(), alert.Message(), str)
	} else {
		log.Printf("(%s) %s", alert.What(), alert.Message())
	}
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

func processAlert(alert lt.Alert) {
	switch alert.What() {
	case "save_resume_data_alert":
		processSaveResumeDataAlert(alert)
		break
	case "metadata_received_alert":
		onMetadataReceived()
		break
	}
}

func consumeAlerts() {
	var alerts lt.StdVectorAlerts
	alerts = session.GetHandle().PopAlerts()
	queueSize := alerts.Size()
	for i := 0; i < int(queueSize); i++ {
		alert := alerts.Get(i)
		logAlert(alert)
		processAlert(alert)
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
			defer lt.DeleteStdVectorChar(resumeData)
			count := 0
			for _, byte := range bytes {
				resumeData.Add(byte)
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
		bootstrap_nodes := ""
		if config.dhtRouters != "" {
			bootstrap_nodes = config.dhtRouters
		} else {
			bootstrap_nodes = strings.Join(dhtBootstrapNodes, ":6881,") + ":6881"
		}
		if bootstrap_nodes != "" {
			log.Println("starting DHT...")
			packSettings.SetStr(lt.SettingByName("dht_bootstrap_nodes"), bootstrap_nodes)
			packSettings.SetBool(lt.SettingByName("enable_dht"), true)
		}
	}
	if config.enableLSD {
		log.Println("starting LSD...")
		packSettings.SetBool(lt.SettingByName("enable_lsd"), true)
	}
	if config.enableUPNP {
		log.Println("starting UPNP...")
		packSettings.SetBool(lt.SettingByName("enable_upnp"), true)
	}
	if config.enableNATPMP {
		log.Println("starting NATPMP...")
		packSettings.SetBool(lt.SettingByName("enable_natpmp"), true)
	}

	session.GetHandle().ApplySettings(packSettings)
}

func startSession() {
	log.Println("starting session...")

	settings := lt.NewSettingsPack()
	session = lt.NewSession(settings, int(lt.SessionHandleAddDefaultPlugins))

	alertMask := int(lt.AlertErrorNotification) | int(lt.AlertStorageNotification) |
		int(lt.AlertTrackerNotification) | int(lt.AlertStatusNotification)
	if config.debugAlerts {
		alertMask |= int(lt.AlertDebugNotification)
	}
	settings.SetInt(lt.SettingByName("alert_mask"), alertMask)

	// settings.SetBool(lt.SettingByName(""))

	settings.SetInt(lt.SettingByName("request_timeout"), config.requestTimeout)
	settings.SetInt(lt.SettingByName("peer_connect_timeout"), config.peerConnectTimeout)
	settings.SetInt(lt.SettingByName("connection_speed"), config.connectionSpeed)
	settings.SetInt(lt.SettingByName("torrent_connect_boost"), config.torrentConnectBoost)

	settings.SetInt(lt.SettingByName("connections_limit"), 0)
	settings.SetInt(lt.SettingByName("download_rate_limit"), 0)
	settings.SetInt(lt.SettingByName("upload_rate_limit"), 0)
	settings.SetBool(lt.SettingByName("strict_end_game_mode"), true)
	settings.SetBool(lt.SettingByName("announce_to_all_trackers"), true)
	settings.SetBool(lt.SettingByName("announce_to_all_tiers"), true)
	settings.SetBool(lt.SettingByName("rate_limit_ip_overhead"), true)
	settings.SetBool(lt.SettingByName("announce_double_nat"), true)
	settings.SetBool(lt.SettingByName("prioritize_partial_pieces"), false)
	settings.SetBool(lt.SettingByName("free_torrent_hashes"), true)
	settings.SetBool(lt.SettingByName("use_parole_mode"), true)
	settings.SetInt(lt.SettingByName("choking_algorithm"), 0)
	settings.SetInt(lt.SettingByName("share_ratio_limit"), 0)
	settings.SetInt(lt.SettingByName("seed_time_ratio_limit"), 0)
	settings.SetInt(lt.SettingByName("seed_time_limit"), 0)
	settings.SetInt(lt.SettingByName("peer_tos"), ipToSLowCost)
	settings.SetInt(lt.SettingByName("seed_choking_algorithm"), int(lt.SettingsPackFastestUpload))
	settings.SetInt(lt.SettingByName("mixed_mode_algorithm"), int(lt.SettingsPackPreferTcp))
	settings.SetBool(lt.SettingByName("no_atime_storage"), true)
	settings.SetBool(lt.SettingByName("upnp_ignore_nonrouters"), true)
	settings.SetBool(lt.SettingByName("lazy_bitfields"), true)
	settings.SetInt(lt.SettingByName("stop_tracker_timeout"), 1)
	settings.SetInt(lt.SettingByName("auto_scrape_interval"), 1200)
	settings.SetInt(lt.SettingByName("auto_scrape_min_interval"), 900)
	settings.SetBool(lt.SettingByName("ignore_limits_on_local_network"), true)
	settings.SetBool(lt.SettingByName("rate_limit_utp"), true)
	settings.SetInt(lt.SettingByName("min_reconnect_time"), config.minReconnectTime)
	settings.SetInt(lt.SettingByName("min_reconnect_time"), config.minReconnectTime)
	settings.SetInt(lt.SettingByName("max_failcount"), config.maxFailCount)

	if config.connectionsLimit >= 0 {
		settings.SetInt(lt.SettingByName("connections_limit"), config.connectionsLimit)
	} else {
		setPlatformSpecificSettings(settings)
	}

	if config.maxDownloadRate >= 0 {
		settings.SetInt(lt.SettingByName("download_rate_limit"), config.maxDownloadRate*1024)
	}
	if config.maxUploadRate >= 0 {
		settings.SetInt(lt.SettingByName("upload_rate_limit"), config.maxUploadRate*1024)
		// If we have an upload rate, use the nicer bittyrant choker
		settings.SetInt(lt.SettingByName("choking_algorithm"), int(lt.SettingsPackBittyrantChoker))
	}

	if config.stateFile != "" {
		log.Printf("loading session state from %s", config.stateFile)
		bytes, err := ioutil.ReadFile(config.stateFile)
		if err != nil {
			log.Println(err)
		} else {
			entry := lt.NewEntry()
			defer lt.DeleteEntry(entry)
			error := lt.Bdecode(string(bytes), entry).(lt.ErrorCode)
			if error.Value() != 0 {
				log.Println(error.Message())
			} else {
				session.GetHandle().LoadState(entry)
			}
		}
	}
	if config.userAgent != "" {
		settings.SetStr(lt.SettingByName("user_agent"), config.userAgent)
	}

	settings.SetBool(lt.SettingByName("enable_incoming_tcp"), config.enableTCP)
	settings.SetBool(lt.SettingByName("enable_outgoing_tcp"), config.enableTCP)
	settings.SetBool(lt.SettingByName("enable_incoming_utp"), config.enableUTP)
	settings.SetBool(lt.SettingByName("enable_outgoing_utp"), config.enableUTP)

	if config.encryption > 0 {
		log.Println("setting encryption settings")

		policy := int(lt.SettingsPackPeDisabled)
		level := int(lt.SettingsPackPeBoth)
		preferRc4 := false

		if config.encryption == 2 {
			policy = int(lt.SettingsPackPeForced)
			level = int(lt.SettingsPackPeRc4)
			preferRc4 = true
		}

		settings.SetInt(lt.SettingByName("out_enc_policy"), policy)
		settings.SetInt(lt.SettingByName("in_enc_policy"), policy)
		settings.SetInt(lt.SettingByName("allowed_enc_level"), level)
		settings.SetBool(lt.SettingByName("prefer_rc4"), preferRc4)
	}

	packSettings = settings
	session.GetHandle().ApplySettings(packSettings)
}

func chooseFile() int {
	biggestFileIndex := int(-1)
	maxSize := int64(0)
	numFiles := torrentInfo.NumFiles()
	candidateFiles := make(map[int]bool)
	files := torrentInfo.Files()

	for i := 0; i < numFiles; i++ {
		size := files.FileSize(i)
		if size > maxSize {
			maxSize = size
			biggestFileIndex = i
		}
		if size > minCandidateSize {
			candidateFiles[i] = true
		}
	}

	log.Printf("there are %d candidate file(s)", len(candidateFiles))

	if config.fileIndex >= 0 {
		if _, ok := candidateFiles[config.fileIndex]; ok {
			log.Printf("selecting requested file at position %d", config.fileIndex)
			return config.fileIndex
		}
		log.Print("unable to select requested file")
	}

	log.Printf("selecting most biggest file (position:%d size:%dkB)", biggestFileIndex, maxSize/1024)
	return biggestFileIndex
}

func pieceFromOffset(offset int64) (int, int64) {
	pieceLength := int64(torrentInfo.PieceLength())
	piece := int(offset / pieceLength)
	pieceOffset := offset % pieceLength
	return piece, pieceOffset
}

func getFilePiecesAndOffset(fe int) (int, int, int64) {
	files := torrentInfo.Files()
	startPiece, offset := pieceFromOffset(files.FileOffset(fe))
	endPiece, _ := pieceFromOffset(files.FileOffset(fe) + files.FileSize(fe))
	return startPiece, endPiece, offset
}

func addTorrent(torrentParams lt.AddTorrentParams) {
	log.Println("adding torrent")
	error := lt.NewErrorCode()
	torrentHandle = session.GetHandle().AddTorrent(torrentParams, error)
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

	fileEntryIdx = chooseFile()

	log.Print("setting piece priorities")

	pieceLength := float64(torrentInfo.PieceLength())
	startPiece, endPiece, _ := getFilePiecesAndOffset(fileEntryIdx)

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
		piecesPriorities.Add(0)
	}
	for _ = 0; curPiece < startPiece+startBufferPieces; curPiece++ { // get this part
		piecesPriorities.Add(7)
		bufferPiecesProgress[curPiece] = 0
		torrentHandle.SetPieceDeadline(curPiece, 0, 0)
	}
	for _ = 0; curPiece < endPiece-endBufferPieces; curPiece++ {
		piecesPriorities.Add(1)
	}
	for _ = 0; curPiece <= endPiece; curPiece++ { // get this part
		piecesPriorities.Add(7)
		bufferPiecesProgress[curPiece] = 0
		torrentHandle.SetPieceDeadline(curPiece, 0, 0)
	}
	numPieces := torrentInfo.NumPieces()
	for _ = 0; curPiece < numPieces; curPiece++ {
		piecesPriorities.Add(0)
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

func handleSignals() {
	forceShutdown = make(chan bool, 1)
	signalChan := make(chan os.Signal, 1)
	saveResumeDataTicker := time.Tick(30 * time.Second)
	signal.Notify(signalChan, os.Interrupt, syscall.SIGTERM)

	for {
		select {
		case <-forceShutdown:
			shutdown()
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

	go handleSignals()
	startHTTP()
}

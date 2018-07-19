package main

import (
	"bytes"
	"crypto/sha1"
	"fmt"
	"io"
	"io/ioutil"
	"net/http"
	"net/url"
	"os"
	"os/exec"
	"path"
	"path/filepath"
	"regexp"
	"strconv"
	"strings"
	"syscall"
	"time"

	log "github.com/Sirupsen/logrus"
	"github.com/julienschmidt/httprouter"
)

const (
	FFMPEGPath       = "ffmpeg"
	root             = "/data/"
	HomeDir          = ".agentVideo"
	cacheDirName     = "cache"
	hlsSegmentLength = 10.0 // Seconds
)

// hhmmssmsToSeconds converts timecode (HH:MM:SS.MS) to seconds (SS.MS).
func hhmmssmsToSeconds(hhmmssms string) float64 {
	var hh, mm, ss, ms float64
	var buffer string
	length := len(hhmmssms)
	timecode := []string{}

	for i := length - 1; i >= 0; i-- {
		if hhmmssms[i] == '.' {
			ms, _ = strconv.ParseFloat(buffer, 64)
			buffer = ""
		} else if hhmmssms[i] == ':' {
			timecode = append(timecode, buffer)
			buffer = ""
		} else if i == 0 {
			if buffer != "" {
				timecode = append(timecode, string(hhmmssms[i])+buffer)
			} else {
				timecode = append(timecode, string(hhmmssms[i]))
			}
		} else {
			buffer = string(hhmmssms[i]) + buffer
		}
	}

	length = len(timecode)

	if length == 1 {
		ss, _ = strconv.ParseFloat(timecode[0], 64)
	} else if length == 2 {
		ss, _ = strconv.ParseFloat(timecode[0], 64)
		mm, _ = strconv.ParseFloat(timecode[1], 64)
	} else if length == 3 {
		ss, _ = strconv.ParseFloat(timecode[0], 64)
		mm, _ = strconv.ParseFloat(timecode[1], 64)
		hh, _ = strconv.ParseFloat(timecode[2], 64)
	}

	return hh*3600 + mm*60 + ss + ms/100
}

func getVideoDuration(path string) (float64, error) {
	con, _ := exec.Command(FFMPEGPath, "-hide_banner", "-i", path).CombinedOutput()
	//	if err != nil {
	//		return 0.0, fmt.Errorf("Error starting command: %v", err)
	//	}
	durationRegex, err := regexp.Compile(`.*Duration: (\d{2}\:\d{2}\:\d{2}\.\d{2}),`)
	if err != nil {
		return 0.0, fmt.Errorf("Get video duration error:%v", err)
	}
	durationStr := strings.Replace(durationRegex.FindString(string(con)), "Duration:", "", 1)
	durationStr = strings.Replace(durationStr, " ", "", -1)
	durationStr = strings.Replace(durationStr, ",", "", -1)

	return hhmmssmsToSeconds(durationStr), nil
}

func urlEncoded(str string) (string, error) {
	u, err := url.Parse(str)
	if err != nil {
		return "", err
	}
	return u.String(), nil
}

func execute(cmdPath string, args []string) (data []byte, err error) {
	cmd := exec.Command(cmdPath, args...)
	stdout, err := cmd.StdoutPipe()
	defer stdout.Close()

	if err != nil {
		err = fmt.Errorf("Error opening stdout of command: %v", err)
		return
	}

	log.Debugf("Executing: %v %v", cmdPath, args)
	err = cmd.Start()
	if err != nil {
		err = fmt.Errorf("Error starting command: %v", err)
		return
	}

	var buffer bytes.Buffer
	_, err = io.Copy(&buffer, stdout)
	if err != nil {
		cmd.Process.Signal(syscall.SIGKILL)
		cmd.Process.Wait()
		err = fmt.Errorf("Error copying stdout to buffer: %v", err)
		return
	}

	err = cmd.Wait()
	if err != nil {
		err = fmt.Errorf("Command failed %v", err)
		return
	}

	data = buffer.Bytes()

	return
}

type EncodingRequest struct {
	file    string
	segment int64
	res     int64
	data    chan *[]byte
	err     chan error
}

func NewEncodingRequest(file string, segment int64, res int64) *EncodingRequest {
	return &EncodingRequest{file, segment, res, make(chan *[]byte, 1), make(chan error, 1)}
}

func NewWarmupEncodingRequest(file string, segment int64, res int64) *EncodingRequest {
	return &EncodingRequest{file, segment, res, nil, nil}
}

func (r *EncodingRequest) sendError(err error) {
	if r.err != nil {
		r.err <- err
	}
}

func (r *EncodingRequest) sendData(data *[]byte) {
	if r.data != nil {
		r.data <- data
	}
}

func (r *EncodingRequest) getCacheKey() string {
	h := sha1.New()
	h.Write([]byte(r.file))
	return fmt.Sprintf("%x.%v.%v", h.Sum(nil), r.res, r.segment)
}

type Encoder struct {
	cacheDir string
	reqChan  chan EncodingRequest
}

func NewEncoder(cacheDir string, workerCount int) *Encoder {
	rc := make(chan EncodingRequest, 100)
	encoder := &Encoder{cacheDir, rc}
	go func() {
		for {
			r := <-rc
			cache, err := encoder.GetFromCache(r)
			if err != nil {
				r.sendError(err)
				continue
			}
			if cache != nil {
				r.sendData(&cache)
				continue
			}
			log.Debugf("Encoding %v:%v", r.file, r.segment)
			data, err := execute(FFMPEGPath, EncodingArgs(r.file, r.segment, r.res))
			if err != nil {
				r.err <- err
				continue
			}
			r.sendData(&data)
			tmp := encoder.GetCacheFile(r) + ".tmp"
			mkerr := os.MkdirAll(filepath.Join(root, HomeDir, encoder.cacheDir), 0777)
			if mkerr != nil {
				log.Errorf("Could not create cache dir")
				continue
			}
			if err2 := ioutil.WriteFile(tmp, data, 0777); err2 == nil {
				os.Rename(tmp, encoder.GetCacheFile(r))
			}
		}
	}()
	return encoder
}

func (e *Encoder) GetFromCache(r EncodingRequest) ([]byte, error) {

	cachePath := e.GetCacheFile(r)
	if _, err := os.Stat(cachePath); err != nil {
		if os.IsNotExist(err) {
			return nil, nil
		}
		return nil, fmt.Errorf("Encoder cache file %v could not be opened because: %v", cachePath, err)
	}
	dat, err := ioutil.ReadFile(cachePath)
	if err != nil {
		return nil, fmt.Errorf("Encoder could not read cache file %v because: %v", cachePath, err)
	}
	return dat, nil
}

func (e *Encoder) GetCacheFile(r EncodingRequest) string {
	return filepath.Join(root, HomeDir, e.cacheDir, r.getCacheKey())
}

func (e *Encoder) Encode(r EncodingRequest) {
	go func() {
		log.Debugf("Encoding requested %v:%v", r.file, r.segment)
		data, err := e.GetFromCache(r)
		if err != nil {
			r.sendError(err)
			return
		}
		if data != nil {
			r.sendData(&data)
			return
		}
		e.reqChan <- r
		e.reqChan <- *NewWarmupEncodingRequest(r.file, r.segment+1, r.res)
		e.reqChan <- *NewWarmupEncodingRequest(r.file, r.segment+2, r.res)
	}()
}

func EncodingArgs(videoFile string, segment int64, res int64) []string {
	startTime := segment * hlsSegmentLength
	var (
		pressTime  int64 = 0
		postssTime int64 = 0
		//offsetTime int64 = 0
	)

	if startTime > 0 {
		pressTime = startTime - 5
		postssTime = 5
		//offsetTime = startTime + hlsSegmentLength + 2
	}

	return []string{
		"-y",
		"-timelimit", "45",
		"-ss", fmt.Sprintf("%v.00", pressTime),
		"-i", videoFile,
		"-ss", fmt.Sprintf("%v.00", postssTime),
		"-t", fmt.Sprintf("%v.00", hlsSegmentLength),
		"-vf", fmt.Sprintf("scale=-2:%v", res),
		"-vcodec", "libx264",
		"-preset", "veryfast",
		"-acodec", "libfdk_aac", //"libvo_aacenc",
		"-pix_fmt", "yuv420p",
		//"-r", "25", // fixed framerate
		//"-vsync", "cfr",
		"-force_key_frames", fmt.Sprintf("expr:gte(t,n_forced*%v.00)", hlsSegmentLength),
		//"-x264opts", "keyint=25:min-keyint=25:scenecut=-1",
		"-f", "ssegment",
		"-segment_time", fmt.Sprintf("%v.00", hlsSegmentLength),
		"-initial_offset", fmt.Sprintf("%v.00", startTime),
		"pipe:out%03d.ts",
	}
}

func Index(w http.ResponseWriter, r *http.Request, _ httprouter.Params) {
	fmt.Fprint(w, "Welcome!\n")
}

func playlist(w http.ResponseWriter, r *http.Request, params httprouter.Params) {
	filename := strings.TrimLeft(params.ByName("filename"), "/filename")
	log.Debugf("Playlist request: %v,%s", r.URL.Path, filename)
	file := path.Join(root, filename)

	duration, err := getVideoDuration(file)
	if err != nil {
		http.Error(w, err.Error(), http.StatusInternalServerError)
		return
	}

	id, err := urlEncoded(filename)
	if err != nil {
		http.Error(w, err.Error(), http.StatusInternalServerError)
		return
	}

	w.Header()["Content-Type"] = []string{"application/vnd.apple.mpegurl"}
	w.Header()["Access-Control-Allow-Origin"] = []string{"*"}

	fmt.Fprint(w, "#EXTM3U\n")
	fmt.Fprint(w, "#EXT-X-VERSION:3\n")
	fmt.Fprint(w, "#EXT-X-MEDIA-SEQUENCE:0\n")
	fmt.Fprint(w, "#EXT-X-ALLOW-CACHE:YES\n")
	fmt.Fprint(w, fmt.Sprintf("#EXT-X-TARGETDURATION:%.f\n", hlsSegmentLength))
	fmt.Fprint(w, "#EXT-X-DISCONTINUITY\n")
	fmt.Fprint(w, "#EXT-X-PLAYLIST-TYPE:VOD\n")

	leftover := duration
	segmentIndex := 0

	for leftover > 0 {
		if leftover > hlsSegmentLength {
			fmt.Fprintf(w, "#EXTINF:%f,\n", hlsSegmentLength)
		} else {
			fmt.Fprintf(w, "#EXTINF:%f,\n", leftover)
		}
		fmt.Fprintf(w, "http://%v/api/hls/segments/%v/%v.ts\n", r.Host, id, segmentIndex)
		segmentIndex++
		leftover = leftover - hlsSegmentLength
	}
	fmt.Fprint(w, "#EXT-X-ENDLIST\n")
}

func hls(w http.ResponseWriter, r *http.Request, params httprouter.Params) {
	filename := strings.TrimLeft(params.ByName("segments"), "/segments")
	log.Debugf("Stream request: %v,%v", r.URL.Path, filename)
	var streamRegexp = regexp.MustCompile(`^(.*)/([0-9]+)\.ts$`)
	matches := streamRegexp.FindStringSubmatch(filename)

	segment, _ := strconv.ParseInt(matches[2], 0, 64)
	file := path.Join(root, matches[1])
	log.Debugf("Stream request: %v,%v", file, segment)

	er := NewEncodingRequest(file, segment, 480)
	NewEncoder("segments", 2).Encode(*er)

	w.Header()["Access-Control-Allow-Origin"] = []string{"*"}
	select {
	case data := <-er.data:
		w.Write(*data)
	case err := <-er.err:
		log.Errorf("Error encoding %v", err)
	case <-time.After(60 * time.Second):
		log.Errorf("Timeout encoding")
	}
}

//获得预览图，待开发
func pic(w http.ResponseWriter, r *http.Request, params httprouter.Params) {
	//filename := strings.Replace(params.ByName("cover"), "/cover/", "", 1)
	log.Debugf("Cover request: %v", r.URL.Path)
}

func main() {
	router := httprouter.New()
	router.GET("/", Index)
	router.GET("/api/playlist/*filename", playlist)
	router.GET("/api/hls/*segments", hls)
	router.GET("/api/pic/*cover", pic)

	log.Fatal(http.ListenAndServe(":8001", router))
}

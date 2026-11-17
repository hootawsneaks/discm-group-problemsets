// Package consumer is the media upload service: a gRPC server that accepts
// simultaneous uploads into a leaky-bucket queue, c worker threads that save
// them, and a web UI that lists and previews the saved videos.
package consumer

import (
	"bytes"
	"embed"
	"encoding/json"
	"fmt"
	"io"
	"log"
	"net"
	"net/http"
	"os"
	"os/exec"
	"path/filepath"
	"sort"
	"strings"
	"sync"
	"sync/atomic"
	"time"

	"p3/internal/config"
	pb "p3/proto"

	"google.golang.org/grpc"
)

//go:embed web/index.html
var webFS embed.FS

const previewSeconds = 10

// Formats browsers can usually play directly. Anything else gets a converted
// H.264 copy so clicking it in the UI actually plays.
var browserPlayable = map[string]bool{".mp4": true, ".webm": true, ".m4v": true}

// job is one fully received video waiting in the queue for a consumer thread.
type job struct {
	filename   string
	producerID int32
	data       []byte
}

type server struct {
	pb.UnimplementedMediaUploadServer
	cfg      config.Config
	queue    chan job // leaky bucket: capacity q, drained by c consumer goroutines
	accepted atomic.Int64
	dropped  atomic.Int64
	saved    atomic.Int64
	nameMu   sync.Mutex // guards picking unique filenames in UploadDir
	ffmpeg   string     // path to ffmpeg, "" if unavailable
}

// UploadVideo receives a client-streamed video. Many of these run concurrently
// (gRPC serves each stream on its own goroutine), so uploads are simultaneous.
// Once the whole file is received it is offered to the queue without blocking;
// if the queue is full the video is dropped (leaky bucket overflow).
func (s *server) UploadVideo(stream pb.MediaUpload_UploadVideoServer) error {
	var (
		name string
		pid  int32
		buf  bytes.Buffer
	)
	for {
		chunk, err := stream.Recv()
		if err == io.EOF {
			break
		}
		if err != nil {
			return err
		}
		if name == "" {
			name = chunk.GetFilename()
			pid = chunk.GetProducerId()
			// Bucket already full: reject now instead of receiving the whole file.
			if len(s.queue) == cap(s.queue) {
				return s.drop(stream, name, pid)
			}
		}
		buf.Write(chunk.GetData())
	}
	if name == "" {
		return stream.SendAndClose(&pb.UploadStatus{Accepted: false, Message: "empty upload"})
	}

	select {
	case s.queue <- job{filename: name, producerID: pid, data: buf.Bytes()}:
		s.accepted.Add(1)
		log.Printf("[recv] queued %q from producer %d (%d bytes) queue=%d/%d",
			name, pid, buf.Len(), len(s.queue), cap(s.queue))
		return stream.SendAndClose(&pb.UploadStatus{Accepted: true, Message: "queued"})
	default:
		return s.drop(stream, name, pid)
	}
}

func (s *server) drop(stream pb.MediaUpload_UploadVideoServer, name string, pid int32) error {
	s.dropped.Add(1)
	log.Printf("[drop] queue full (%d/%d), dropped %q from producer %d",
		len(s.queue), cap(s.queue), name, pid)
	return stream.SendAndClose(&pb.UploadStatus{Accepted: false, Message: "queue full, video dropped"})
}

// worker is one consumer thread: it takes videos off the queue, saves them to
// the upload folder and generates the 10-second preview (plus a playable copy
// if the original format won't play in a browser).
func (s *server) worker(id int) {
	for j := range s.queue {
		path, err := s.save(j)
		if err != nil {
			log.Printf("[consumer %d] save %q failed: %v", id, j.filename, err)
			continue
		}
		s.saved.Add(1)
		log.Printf("[consumer %d] saved %s", id, path)
		if s.ffmpeg == "" {
			continue
		}
		if err := s.makePreview(path); err != nil {
			log.Printf("[consumer %d] preview for %s failed: %v", id, filepath.Base(path), err)
		}
		if !browserPlayable[strings.ToLower(filepath.Ext(path))] {
			if err := s.makePlayable(path); err != nil {
				log.Printf("[consumer %d] convert %s failed: %v", id, filepath.Base(path), err)
			} else {
				log.Printf("[consumer %d] converted %s to mp4 for browser playback", id, filepath.Base(path))
			}
		}
	}
}

// save writes the video to a temp file and renames it into place, so the
// web UI never lists a half-written file. Name collisions get a _N suffix.
func (s *server) save(j job) (string, error) {
	base := filepath.Base(j.filename)
	if base == "." || base == string(filepath.Separator) || base == "" {
		base = fmt.Sprintf("video_%d.mp4", time.Now().UnixNano())
	}
	ext := filepath.Ext(base)
	stem := strings.TrimSuffix(base, ext)

	tmp, err := os.CreateTemp(s.cfg.UploadDir, ".part-*")
	if err != nil {
		return "", err
	}
	if _, err := tmp.Write(j.data); err != nil {
		tmp.Close()
		os.Remove(tmp.Name())
		return "", err
	}
	if err := tmp.Close(); err != nil {
		os.Remove(tmp.Name())
		return "", err
	}

	s.nameMu.Lock()
	defer s.nameMu.Unlock()
	dest := filepath.Join(s.cfg.UploadDir, base)
	for i := 1; ; i++ {
		if _, err := os.Stat(dest); os.IsNotExist(err) {
			break
		}
		dest = filepath.Join(s.cfg.UploadDir, fmt.Sprintf("%s_%d%s", stem, i, ext))
	}
	return dest, os.Rename(tmp.Name(), dest)
}

// makePreview encodes the first 10 seconds as a small, muted mp4.
func (s *server) makePreview(videoPath string) error {
	out := filepath.Join(s.cfg.PreviewDir, filepath.Base(videoPath)+".mp4")
	return s.encode(videoPath, out,
		"-t", fmt.Sprint(previewSeconds), "-an", "-vf", "scale=480:-2")
}

// makePlayable encodes the full video as H.264/AAC mp4 so browsers can play it.
func (s *server) makePlayable(videoPath string) error {
	out := filepath.Join(s.cfg.ConvertDir, filepath.Base(videoPath)+".mp4")
	return s.encode(videoPath, out, "-c:a", "aac", "-vf", "scale=trunc(iw/2)*2:trunc(ih/2)*2")
}

// encode runs ffmpeg into a temp file and renames it into place when done.
func (s *server) encode(in, out string, extra ...string) error {
	tmp := out + ".part.mp4"
	args := []string{"-y", "-loglevel", "error", "-i", in}
	args = append(args, extra...)
	args = append(args, "-c:v", "libx264", "-preset", "veryfast",
		"-pix_fmt", "yuv420p", "-movflags", "+faststart", tmp)
	if outp, err := exec.Command(s.ffmpeg, args...).CombinedOutput(); err != nil {
		os.Remove(tmp)
		return fmt.Errorf("%v: %s", err, strings.TrimSpace(string(outp)))
	}
	return os.Rename(tmp, out)
}

// ---- web UI ----

type videoInfo struct {
	Name     string `json:"name"`
	Size     int64  `json:"size"`
	URL      string `json:"url"`               // original upload
	Play     string `json:"play"`              // what the player should load on click
	Playable bool   `json:"playable"`          // false if the browser likely can't play Play
	Preview  string `json:"preview,omitempty"` // 10-second preview, once generated
	ModTime  int64  `json:"mtime"`
}

func exists(path string) bool {
	_, err := os.Stat(path)
	return err == nil
}

func (s *server) listVideos(w http.ResponseWriter, r *http.Request) {
	entries, err := os.ReadDir(s.cfg.UploadDir)
	if err != nil {
		http.Error(w, err.Error(), 500)
		return
	}
	videos := []videoInfo{}
	for _, e := range entries {
		if e.IsDir() || strings.HasPrefix(e.Name(), ".") {
			continue
		}
		info, err := e.Info()
		if err != nil {
			continue
		}
		name := e.Name()
		v := videoInfo{Name: name, Size: info.Size(), URL: "/videos/" + name, ModTime: info.ModTime().UnixMilli()}
		v.Play, v.Playable = v.URL, browserPlayable[strings.ToLower(filepath.Ext(name))]
		if exists(filepath.Join(s.cfg.ConvertDir, name+".mp4")) {
			v.Play, v.Playable = "/converted/"+name+".mp4", true
		}
		if exists(filepath.Join(s.cfg.PreviewDir, name+".mp4")) {
			v.Preview = "/previews/" + name + ".mp4"
		}
		videos = append(videos, v)
	}
	sort.Slice(videos, func(i, j int) bool { return videos[i].ModTime > videos[j].ModTime })

	w.Header().Set("Content-Type", "application/json")
	json.NewEncoder(w).Encode(map[string]any{
		"videos":    videos,
		"queue_len": len(s.queue),
		"queue_cap": cap(s.queue),
		"accepted":  s.accepted.Load(),
		"dropped":   s.dropped.Load(),
		"saved":     s.saved.Load(),
		"consumers": s.cfg.Consumers,
	})
}

// Run starts the consumer: c worker threads, the web UI and the gRPC server.
func Run(cfg config.Config) error {
	for _, d := range []string{cfg.UploadDir, cfg.PreviewDir, cfg.ConvertDir} {
		if err := os.MkdirAll(d, 0o755); err != nil {
			return err
		}
	}
	s := &server{cfg: cfg, queue: make(chan job, cfg.QueueSize)}
	if p, err := exec.LookPath("ffmpeg"); err == nil {
		s.ffmpeg = p
	} else {
		log.Printf("ffmpeg not found: previews will play the first %ds of the original file instead, "+
			"and non-mp4/webm videos may not play in the browser", previewSeconds)
	}

	for i := 1; i <= cfg.Consumers; i++ {
		go s.worker(i)
	}

	// Web UI
	mux := http.NewServeMux()
	mux.HandleFunc("/api/videos", s.listVideos)
	mux.Handle("/videos/", http.StripPrefix("/videos/", http.FileServer(http.Dir(cfg.UploadDir))))
	mux.Handle("/previews/", http.StripPrefix("/previews/", http.FileServer(http.Dir(cfg.PreviewDir))))
	mux.Handle("/converted/", http.StripPrefix("/converted/", http.FileServer(http.Dir(cfg.ConvertDir))))
	mux.HandleFunc("/", func(w http.ResponseWriter, r *http.Request) {
		if r.URL.Path != "/" {
			http.NotFound(w, r)
			return
		}
		page, _ := webFS.ReadFile("web/index.html")
		w.Header().Set("Content-Type", "text/html; charset=utf-8")
		w.Write(page)
	})
	go func() {
		log.Printf("web UI on %s", cfg.HTTPAddr)
		if err := http.ListenAndServe(cfg.HTTPAddr, mux); err != nil {
			log.Fatalf("http: %v", err)
		}
	}()

	// gRPC
	lis, err := net.Listen("tcp", cfg.ListenAddr)
	if err != nil {
		return err
	}
	gs := grpc.NewServer(grpc.MaxRecvMsgSize(16 << 20))
	pb.RegisterMediaUploadServer(gs, s)
	log.Printf("consumer: gRPC listening on %s, c=%d consumer threads, q=%d max queue, saving to %s/",
		lis.Addr(), cfg.Consumers, cfg.QueueSize, cfg.UploadDir)
	return gs.Serve(lis)
}

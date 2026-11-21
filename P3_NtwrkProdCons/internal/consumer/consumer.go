// Package consumer is the media upload service: a gRPC server that accepts
// simultaneous uploads into a leaky-bucket queue, c worker threads that save
// them, and a web UI (over HTTPS) that lists and previews the saved videos.
package consumer

import (
	"crypto/sha256"
	"embed"
	"encoding/hex"
	"encoding/json"
	"fmt"
	"hash"
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
	"p3/internal/tlsgen"
	pb "p3/proto"

	"google.golang.org/grpc"
)

//go:embed web/index.html
var webFS embed.FS

const previewSeconds = 10

// Formats browsers can usually play directly. Anything else must have a
// converted H.264 copy before clicking it in the UI will play.
var browserPlayable = map[string]bool{".mp4": true, ".webm": true, ".m4v": true}

// pending marks a hash that is reserved by an upload still in flight.
const pending = ""

// job is one fully received video waiting in the queue for a consumer thread.
// The bytes are already on disk in tmpPath, so a full queue costs q file
// handles instead of q whole videos in memory.
type job struct {
	filename   string
	producerID int32
	tmpPath    string
	size       int64
	sum        string // SHA-256 of the contents, for duplicate detection
}

type server struct {
	pb.UnimplementedMediaUploadServer
	cfg        config.Config
	queue      chan job // leaky bucket: capacity q, drained by c consumer goroutines
	accepted   atomic.Int64
	dropped    atomic.Int64
	duplicates atomic.Int64
	saved      atomic.Int64
	bytesSaved atomic.Int64 // bytes the compressed copies shaved off

	nameMu sync.Mutex // guards picking unique filenames in UploadDir

	hashMu sync.Mutex        // guards seen
	seen   map[string]string // SHA-256 -> saved filename ("" while still in flight)

	ffmpeg string // path to ffmpeg, "" if unavailable
}

// UploadVideo receives a client-streamed video. Many of these run concurrently
// (gRPC serves each stream on its own goroutine), so uploads are simultaneous.
// Chunks are written straight to a temp file; once the whole file is received
// it is offered to the queue without blocking. If the queue is full the video
// is dropped (leaky bucket overflow), and if an identical video was already
// uploaded it is reported as a duplicate.
func (s *server) UploadVideo(stream pb.MediaUpload_UploadVideoServer) error {
	var (
		name string
		pid  int32
		tmp  *os.File
		sum  hash.Hash
		size int64
	)
	// Nothing is left behind if the stream fails halfway.
	defer func() {
		if tmp != nil {
			tmp.Close()
			os.Remove(tmp.Name())
		}
	}()

	for {
		chunk, err := stream.Recv()
		if err == io.EOF {
			break
		}
		if err != nil {
			return err
		}
		if tmp == nil { // first chunk carries the metadata
			name, pid = chunk.GetFilename(), chunk.GetProducerId()
			// Bucket already full: reject now instead of receiving the whole file.
			if len(s.queue) == cap(s.queue) {
				return s.drop(stream, name, pid)
			}
			if tmp, err = os.CreateTemp(s.cfg.UploadDir, ".part-*"); err != nil {
				return err
			}
			sum = sha256.New()
		}
		data := chunk.GetData()
		if _, err := tmp.Write(data); err != nil {
			return err
		}
		sum.Write(data)
		size += int64(len(data))
	}
	if tmp == nil {
		return stream.SendAndClose(&pb.UploadStatus{Accepted: false, Message: "empty upload"})
	}
	if err := tmp.Close(); err != nil {
		return err
	}

	digest := hex.EncodeToString(sum.Sum(nil))
	if existing, reserved := s.reserveHash(digest); !reserved {
		s.duplicates.Add(1)
		log.Printf("[dup] %q from producer %d is a duplicate of %s", name, pid, existing)
		return stream.SendAndClose(&pb.UploadStatus{
			Accepted: false, Message: "duplicate of " + existing,
		})
	}

	j := job{filename: name, producerID: pid, tmpPath: tmp.Name(), size: size, sum: digest}
	select {
	case s.queue <- j:
		tmp = nil // the worker owns the file now; don't delete it
		s.accepted.Add(1)
		log.Printf("[recv] queued %q from producer %d (%d bytes) queue=%d/%d",
			name, pid, size, len(s.queue), cap(s.queue))
		return stream.SendAndClose(&pb.UploadStatus{Accepted: true, Message: "queued"})
	default:
		s.releaseHash(digest)
		return s.drop(stream, name, pid)
	}
}

func (s *server) drop(stream pb.MediaUpload_UploadVideoServer, name string, pid int32) error {
	s.dropped.Add(1)
	log.Printf("[drop] queue full (%d/%d), dropped %q from producer %d",
		len(s.queue), cap(s.queue), name, pid)
	return stream.SendAndClose(&pb.UploadStatus{Accepted: false, Message: "queue full, video dropped"})
}

// ---- duplicate detection (bonus) ----

// reserveHash claims digest for the caller. reserved is false when an
// identical video is already saved or in flight, and existing then names it.
func (s *server) reserveHash(digest string) (existing string, reserved bool) {
	s.hashMu.Lock()
	defer s.hashMu.Unlock()
	if name, ok := s.seen[digest]; ok {
		if name == pending {
			name = "an upload already in progress"
		}
		return name, false
	}
	s.seen[digest] = pending
	return "", true
}

// releaseHash gives up a reservation, so a dropped video can be sent again.
func (s *server) releaseHash(digest string) {
	s.hashMu.Lock()
	defer s.hashMu.Unlock()
	if s.seen[digest] == pending {
		delete(s.seen, digest)
	}
}

func (s *server) nameHash(digest, name string) {
	s.hashMu.Lock()
	defer s.hashMu.Unlock()
	s.seen[digest] = name
}

// scanExisting hashes whatever is already in the upload folder, so restarting
// the consumer does not forget which videos it has.
func (s *server) scanExisting() {
	entries, err := os.ReadDir(s.cfg.UploadDir)
	if err != nil {
		return
	}
	for _, e := range entries {
		if e.IsDir() || strings.HasPrefix(e.Name(), ".") {
			continue
		}
		f, err := os.Open(filepath.Join(s.cfg.UploadDir, e.Name()))
		if err != nil {
			continue
		}
		h := sha256.New()
		_, err = io.Copy(h, f)
		f.Close()
		if err == nil {
			s.nameHash(hex.EncodeToString(h.Sum(nil)), e.Name())
		}
	}
	if n := len(s.seen); n > 0 {
		log.Printf("duplicate detection: %d video(s) already in %s", n, s.cfg.UploadDir)
	}
}

// ---- consumer threads ----

// worker is one consumer thread: it takes videos off the queue, saves them to
// the upload folder, generates the 10-second preview and a smaller compressed
// copy for the browser to play.
func (s *server) worker(id int) {
	for j := range s.queue {
		path, err := s.save(j)
		if err != nil {
			log.Printf("[consumer %d] save %q failed: %v", id, j.filename, err)
			s.releaseHash(j.sum)
			os.Remove(j.tmpPath)
			continue
		}
		s.nameHash(j.sum, filepath.Base(path))
		s.saved.Add(1)
		log.Printf("[consumer %d] saved %s (%s)", id, path, humanBytes(j.size))
		if s.ffmpeg == "" {
			continue
		}
		if err := s.makePreview(path); err != nil {
			log.Printf("[consumer %d] preview for %s failed: %v", id, filepath.Base(path), err)
		}
		if s.cfg.Compress {
			s.compress(id, path, j.size)
		}
	}
}

// save renames the received temp file into place under a unique name, so the
// web UI never lists a half-written file. Name collisions get a _N suffix.
func (s *server) save(j job) (string, error) {
	base := filepath.Base(j.filename)
	if base == "." || base == string(filepath.Separator) || base == "" {
		base = fmt.Sprintf("video_%d.mp4", time.Now().UnixNano())
	}
	ext := filepath.Ext(base)
	stem := strings.TrimSuffix(base, ext)

	s.nameMu.Lock()
	defer s.nameMu.Unlock()
	dest := filepath.Join(s.cfg.UploadDir, base)
	for i := 1; ; i++ {
		if _, err := os.Stat(dest); os.IsNotExist(err) {
			break
		}
		dest = filepath.Join(s.cfg.UploadDir, fmt.Sprintf("%s_%d%s", stem, i, ext))
	}
	return dest, os.Rename(j.tmpPath, dest)
}

// ---- ffmpeg work ----

// makePreview encodes the first 10 seconds as a small, muted mp4.
func (s *server) makePreview(videoPath string) error {
	out := filepath.Join(s.cfg.PreviewDir, filepath.Base(videoPath)+".mp4")
	return s.encode(videoPath, out,
		"-t", fmt.Sprint(previewSeconds), "-an", "-vf", "scale=480:-2")
}

// compress writes a smaller H.264/AAC copy, which is what the GUI plays. If the
// copy ends up no smaller and the original already plays in browsers, it is
// thrown away and the original is played instead.
func (s *server) compress(workerID int, videoPath string, originalSize int64) {
	name := filepath.Base(videoPath)
	out := filepath.Join(s.cfg.ConvertDir, name+".mp4")
	err := s.encode(videoPath, out,
		"-crf", fmt.Sprint(s.cfg.CompressCRF),
		"-c:a", "aac", "-vf", "scale=trunc(iw/2)*2:trunc(ih/2)*2")
	if err != nil {
		log.Printf("[consumer %d] compress %s failed: %v", workerID, name, err)
		return
	}
	st, err := os.Stat(out)
	if err != nil {
		return
	}
	gain := originalSize - st.Size()
	if gain <= 0 && browserPlayable[strings.ToLower(filepath.Ext(name))] {
		os.Remove(out)
		log.Printf("[consumer %d] %s is already well compressed, keeping the original", workerID, name)
		return
	}
	if gain > 0 {
		s.bytesSaved.Add(gain)
	}
	log.Printf("[consumer %d] compressed %s: %s -> %s (%.1f%% smaller, crf %d)",
		workerID, name, humanBytes(originalSize), humanBytes(st.Size()),
		100*float64(gain)/float64(max64(originalSize, 1)), s.cfg.CompressCRF)
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

func max64(a, b int64) int64 {
	if a > b {
		return a
	}
	return b
}

func humanBytes(n int64) string {
	const unit = 1024
	if n < unit {
		return fmt.Sprintf("%d B", n)
	}
	v := float64(n)
	for _, u := range []string{"KB", "MB", "GB", "TB"} {
		v /= unit
		if v < unit {
			return fmt.Sprintf("%.1f %s", v, u)
		}
	}
	return fmt.Sprintf("%.1f PB", v/unit)
}

// ---- web UI ----

type videoInfo struct {
	Name     string `json:"name"`
	Size     int64  `json:"size"`
	URL      string `json:"url"`               // original upload
	Play     string `json:"play"`              // what the player should load on click
	Playable bool   `json:"playable"`          // false if the browser likely cannot play Play
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
		"videos":      videos,
		"queue_len":   len(s.queue),
		"queue_cap":   cap(s.queue),
		"accepted":    s.accepted.Load(),
		"dropped":     s.dropped.Load(),
		"duplicates":  s.duplicates.Load(),
		"saved":       s.saved.Load(),
		"bytes_saved": s.bytesSaved.Load(),
		"consumers":   s.cfg.Consumers,
	})
}

func (s *server) routes() *http.ServeMux {
	mux := http.NewServeMux()
	mux.HandleFunc("/api/videos", s.listVideos)
	mux.Handle("/videos/", http.StripPrefix("/videos/", http.FileServer(http.Dir(s.cfg.UploadDir))))
	mux.Handle("/previews/", http.StripPrefix("/previews/", http.FileServer(http.Dir(s.cfg.PreviewDir))))
	mux.Handle("/converted/", http.StripPrefix("/converted/", http.FileServer(http.Dir(s.cfg.ConvertDir))))
	mux.HandleFunc("/", func(w http.ResponseWriter, r *http.Request) {
		if r.URL.Path != "/" {
			http.NotFound(w, r)
			return
		}
		page, _ := webFS.ReadFile("web/index.html")
		w.Header().Set("Content-Type", "text/html; charset=utf-8")
		w.Write(page)
	})
	return mux
}

// Run starts the consumer: c worker threads, the HTTPS web UI and the gRPC server.
func Run(cfg config.Config) error {
	for _, d := range []string{cfg.UploadDir, cfg.PreviewDir, cfg.ConvertDir} {
		if err := os.MkdirAll(d, 0o755); err != nil {
			return err
		}
	}
	s := &server{cfg: cfg, queue: make(chan job, cfg.QueueSize), seen: map[string]string{}}
	s.scanExisting()
	if p, err := exec.LookPath("ffmpeg"); err == nil {
		s.ffmpeg = p
	} else {
		log.Printf("ffmpeg not found: previews will play the first %ds of the original file instead, "+
			"compression is off, and non-mp4/webm videos may not play in the browser", previewSeconds)
	}

	for i := 1; i <= cfg.Consumers; i++ {
		go s.worker(i)
	}

	// Web UI over HTTPS, with a self-signed certificate made on first start.
	hosts, err := tlsgen.EnsureCert(cfg.TLSCert, cfg.TLSKey, cfg.TLSHosts)
	if err != nil {
		return fmt.Errorf("tls certificate: %w", err)
	}
	go func() {
		log.Printf("web UI on %s (self-signed certificate for %s; the browser will warn once)",
			browserURL(cfg.HTTPAddr), strings.Join(hosts, ", "))
		// ServeHTTPS also redirects plain http:// on this port, so typing
		// "localhost:8080" lands on the page instead of a TLS error.
		if err := tlsgen.ServeHTTPS(cfg.HTTPAddr, cfg.TLSCert, cfg.TLSKey, s.routes()); err != nil {
			log.Fatalf("https: %v", err)
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

// browserURL turns a listen address like ":8080" into something clickable.
func browserURL(addr string) string {
	host, port, err := net.SplitHostPort(addr)
	if err != nil {
		return "https://" + addr
	}
	if host == "" || host == "0.0.0.0" || host == "::" {
		host = "localhost"
	}
	return "https://" + net.JoinHostPort(host, port)
}

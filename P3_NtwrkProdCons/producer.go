package main

import (
	"context"
	"fmt"
	"io"
	"log"
	"os"
	"path/filepath"
	"strings"
	"sync"
	"sync/atomic"

	pb "p3/proto"

	"google.golang.org/grpc"
	"google.golang.org/grpc/credentials/insecure"
)

const chunkSize = 64 * 1024

var videoExts = map[string]bool{
	".mp4": true, ".mov": true, ".mkv": true, ".webm": true, ".avi": true, ".m4v": true,
}

// runProducer starts p producer threads. Producer i uploads every video in
// <input_dir>/p<i> to the consumer over gRPC.
func runProducer(cfg Config) error {
	conn, err := grpc.NewClient(cfg.Address, grpc.WithTransportCredentials(insecure.NewCredentials()))
	if err != nil {
		return err
	}
	defer conn.Close()
	client := pb.NewMediaUploadClient(conn)

	var accepted, dropped, failed atomic.Int64
	var wg sync.WaitGroup
	for i := 1; i <= cfg.Producers; i++ {
		dir := filepath.Join(cfg.InputDir, fmt.Sprintf("p%d", i))
		wg.Add(1)
		go func(id int, dir string) {
			defer wg.Done()
			files, err := listVideos(dir)
			if err != nil {
				log.Printf("[producer %d] cannot read %s: %v", id, dir, err)
				return
			}
			log.Printf("[producer %d] %d video(s) in %s", id, len(files), dir)
			for _, f := range files {
				st, err := uploadFile(context.Background(), client, int32(id), f)
				switch {
				case err != nil:
					failed.Add(1)
					log.Printf("[producer %d] %s: error: %v", id, filepath.Base(f), err)
				case st.GetAccepted():
					accepted.Add(1)
					log.Printf("[producer %d] %s: accepted (%s)", id, filepath.Base(f), st.GetMessage())
				default:
					dropped.Add(1)
					log.Printf("[producer %d] %s: DROPPED (%s)", id, filepath.Base(f), st.GetMessage())
				}
			}
		}(i, dir)
	}
	wg.Wait()
	log.Printf("done: %d accepted, %d dropped, %d failed", accepted.Load(), dropped.Load(), failed.Load())
	return nil
}

func listVideos(dir string) ([]string, error) {
	entries, err := os.ReadDir(dir)
	if err != nil {
		return nil, err
	}
	var out []string
	for _, e := range entries {
		if !e.IsDir() && videoExts[strings.ToLower(filepath.Ext(e.Name()))] {
			out = append(out, filepath.Join(dir, e.Name()))
		}
	}
	return out, nil
}

// uploadFile streams one video to the consumer in chunks.
func uploadFile(ctx context.Context, client pb.MediaUploadClient, producerID int32, path string) (*pb.UploadStatus, error) {
	f, err := os.Open(path)
	if err != nil {
		return nil, err
	}
	defer f.Close()

	stream, err := client.UploadVideo(ctx)
	if err != nil {
		return nil, err
	}
	name := filepath.Base(path)
	buf := make([]byte, chunkSize)
	for {
		n, rerr := f.Read(buf)
		if n > 0 {
			if err := stream.Send(&pb.VideoChunk{Filename: name, ProducerId: producerID, Data: buf[:n]}); err != nil {
				// io.EOF means the consumer already answered (e.g. dropped early);
				// the real status comes from CloseAndRecv below.
				if err == io.EOF {
					break
				}
				return nil, err
			}
		}
		if rerr == io.EOF {
			break
		}
		if rerr != nil {
			return nil, rerr
		}
	}
	return stream.CloseAndRecv()
}

package buffer

import (
	"bufio"
	"bytes"
	"context"
	"errors"
	"fmt"
	"io"
	"m3u-stream-merger/proxy"
	"m3u-stream-merger/proxy/loadbalancer"
	"m3u-stream-merger/utils"
	"math"
	"math/rand/v2"
	"net/http"
	"net/url"
	"strings"
	"sync/atomic"
	"time"
)

type PlaylistMetadata struct {
	TargetDuration float64
	MediaSequence  int64
	Version        int
	IsEndlist      bool
	Segments       []string
	IsMaster       bool
}

func (c *StreamCoordinator) StartHLSWriter(ctx context.Context, lbResult *loadbalancer.LoadBalancerResult) {
	defer func() {
		c.LBResultOnWrite.Store(nil)
		if r := recover(); r != nil {
			c.logger.Errorf("Panic in StartHLSWriter: %v", r)
			c.writeError(fmt.Errorf("internal server error"), proxy.StatusServerError)
		}
	}()

	c.LBResultOnWrite.Store(lbResult)
	c.WriterRespHeader.Store(nil)
	c.respHeaderSet = make(chan struct{})
	c.m3uHeaderSet.Store(false)
	c.logger.Debug("StartHLSWriter: Beginning read loop")

	c.cm.UpdateConcurrency(lbResult.Index, true)
	defer c.cm.UpdateConcurrency(lbResult.Index, false)

	var lastErr error
	lastChangeTime := time.Now()
	lastMediaSeq := int64(-1)

	// Start with a conservative polling rate
	pollInterval := time.Second
	ticker := time.NewTicker(pollInterval)
	defer ticker.Stop()

	for atomic.LoadInt32(&c.state) == stateActive {
		select {
		case <-ctx.Done():
			if lastErr == nil {
				c.logger.Debug("StartHLSWriter: Context cancelled")
				c.writeError(ctx.Err(), proxy.StatusClientClosed)
			}
			return
		case <-ticker.C:
			// Check timeout first
			if time.Since(lastChangeTime) > time.Duration(c.config.TimeoutSeconds)*time.Second+pollInterval {
				fmt.Printf("Last Change Time %s  , Limit = %s \n", lastChangeTime, time.Duration(c.config.TimeoutSeconds)*time.Second+pollInterval)
				c.logger.Debug("No sequence changes detected within timeout period")
				c.writeError(fmt.Errorf("stream timeout: no new segments"), proxy.StatusEOF)
				return
			}

			// Copy response body for fallback
			httpRequestBody := &bytes.Buffer{}
			_, err := io.Copy(httpRequestBody, lbResult.Response.Body)
			if err != nil {
				c.writeError(err, proxy.StatusServerError)
				return
			}

			lbResult.Response.Body.Close()

			byteClone := bytes.Clone(httpRequestBody.Bytes())
			requestBodyClone := bytes.NewBuffer(byteClone)

			lbResult.Response.Body = io.NopCloser(httpRequestBody)
			m3uPlaylist, err := io.ReadAll(requestBodyClone)
			if err != nil {
				c.writeError(err, proxy.StatusServerError)
				return
			}

			mediaURL := lbResult.Response.Request.URL.String()
			metadata, err := c.parsePlaylist(mediaURL, string(m3uPlaylist))
			if err != nil {
				c.writeError(err, proxy.StatusServerError)
				return
			}

			// Update polling rate based on target duration
			if metadata.TargetDuration > 0 {
				// HLS spec recommends polling at no less than target duration / 2
				newInterval := time.Duration(metadata.TargetDuration * float64(time.Second) / 2)

				// Add a small random jitter (±10%) to prevent thundering herd
				jitter := time.Duration(float64(newInterval) * (0.9 + 0.2*rand.Float64()))

				// Only update if significantly different (>10% change)
				if math.Abs(float64(jitter-pollInterval)) > float64(pollInterval)*0.1 {
					pollInterval = jitter
					ticker.Reset(pollInterval)
					c.logger.Debugf("Updated polling interval to %v", pollInterval)
				}
			}

			if metadata.IsMaster {
				c.writeError(fmt.Errorf("master playlist not supported"), proxy.StatusServerError)
				return
			}

			if metadata.IsEndlist {
				// Process remaining segments before ending
				err = c.processSegments(ctx, metadata.Segments)
				if err != nil {
					c.logger.Errorf("Error processing segments: %v", err)
				}
				c.writeError(io.EOF, proxy.StatusEOF)
				return
			}
			fmt.Printf("Media Seq : %d \n", metadata.MediaSequence)
			if metadata.MediaSequence >= lastMediaSeq {
				lastChangeTime = time.Now()

				lastMediaSeq = metadata.MediaSequence

				if err := c.processSegments(ctx, metadata.Segments); err != nil {
					if ctx.Err() != nil {
						c.writeError(err, proxy.StatusServerError)
						return
					}
					lastErr = err
				}
			}

		}
	}
}

func (c *StreamCoordinator) processSegments(ctx context.Context, segments []string) error {
	for _, segment := range segments {
		select {
		case <-ctx.Done():
			return ctx.Err()
		default:
			if err := c.streamSegment(ctx, segment); err != nil {
				if err != io.EOF {
					return err
				}
			}
		}
	}
	return nil
}

func (c *StreamCoordinator) streamSegment(ctx context.Context, segmentURL string) error {
	req, err := http.NewRequest("GET", segmentURL, nil)
	if err != nil {
		return fmt.Errorf("Error creating request to segment: %v", err)
	}

	resp, err := utils.HTTPClient.Do(req)
	if err != nil {
		return fmt.Errorf("Error fetching segment stream: %v", err)
	}

	if resp == nil {
		return errors.New("Returned nil response from HTTP client")
	}
	defer resp.Body.Close()

	if resp.StatusCode != http.StatusOK {
		return fmt.Errorf("Non-200 status code received: %d for %s", resp.StatusCode, segmentURL)
	}

	contentType := resp.Header.Get("Content-Type")
	if contentType == "" {
		resp.Header.Set("Content-Type", "video/MP2T")
	}

	if c.m3uHeaderSet.CompareAndSwap(false, true) {
		resp.Header.Del("Content-Length")
		c.WriterRespHeader.Store(&resp.Header)
		close(c.respHeaderSet)
	}

	return c.readAndWriteStream(ctx, resp.Body, func(b []byte) error {
		chunk := newChunkData()
		_, _ = chunk.Buffer.Write(b)
		chunk.Timestamp = time.Now()
		if !c.Write(chunk) {
			chunk.Reset()
		}
		return nil
	})

}

func (c *StreamCoordinator) parsePlaylist(mediaURL string, content string) (*PlaylistMetadata, error) {
	metadata := &PlaylistMetadata{
		Segments:       make([]string, 0, 32),
		TargetDuration: 2, // Default target duration as fallback
	}

	base, err := url.Parse(mediaURL)
	if err != nil {
		return nil, fmt.Errorf("failed to parse base URL: %w", err)
	}

	scanner := bufio.NewScanner(strings.NewReader(content))
	for scanner.Scan() {
		line := strings.TrimSpace(scanner.Text())

		switch {
		case strings.HasPrefix(line, "#EXTM3U"):
			continue
		case strings.HasPrefix(line, "#EXT-X-STREAM-INF"):
			continue
		case strings.HasPrefix(line, "#EXT-X-VERSION:"):
			_, _ = fmt.Sscanf(line, "#EXT-X-VERSION:%d", &metadata.Version)
		case strings.HasPrefix(line, "#EXT-X-TARGETDURATION:"):
			_, _ = fmt.Sscanf(line, "#EXT-X-TARGETDURATION:%f", &metadata.TargetDuration)
		case strings.HasPrefix(line, "#EXT-X-MEDIA-SEQUENCE:"):
			_, _ = fmt.Sscanf(line, "#EXT-X-MEDIA-SEQUENCE:%d", &metadata.MediaSequence)
		case line == "#EXT-X-ENDLIST":
			metadata.IsEndlist = true
		case !strings.HasPrefix(line, "#") && line != "":
			segURL, err := url.Parse(line)
			if err != nil {
				c.logger.Warnf("Invalid segment URL %q: %v", line, err)
				continue
			}

			if !segURL.IsAbs() {
				segURL = base.ResolveReference(segURL)
			}
			metadata.Segments = append(metadata.Segments, segURL.String())
		}
	}

	return metadata, scanner.Err()
}

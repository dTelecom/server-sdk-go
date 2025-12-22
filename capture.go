package lksdk

import (
	"context"
	"encoding/binary"
	"fmt"

	"time"

	"github.com/dtelecom/server-sdk-go/pkg/samplebuilder"
	"github.com/pion/rtp/codecs"
	"github.com/pion/webrtc/v3"
)

func MakeScreenshot(track *webrtc.TrackRemote, codec string) ([]byte, error) {
	switch codec {
	case webrtc.MimeTypeVP8:
		return makeScreenshotVP8(track)
	case webrtc.MimeTypeH264:
		return makeScreenshotH264(track)
	default:
		return nil, fmt.Errorf("unsupported codec name: %s", codec)
	}
}

// ===== VP8 =====
func makeScreenshotVP8(track *webrtc.TrackRemote) ([]byte, error) {
	return getKeyFrameVP8(track)
}

func getKeyFrameVP8(track *webrtc.TrackRemote) ([]byte, error) {
	const clock = 90000
	sb := samplebuilder.New(200, &codecs.VP8Packet{}, clock)

	ctx, cancel := context.WithCancel(context.Background())
	defer cancel() // Ensure goroutine is stopped when function exits

	errCh := make(chan error, 1)
	go func() {
		defer func () {
			if r := recover(); r != nil {
				logger.Errorw("gorouting crached", fmt.Errorf("%+v\n%s", r, funcName(2)))
			}
		}()
		defer close(errCh)
		for {
			select {
			case <-ctx.Done():
				return
			default:
			}

			pkt, _, err := track.ReadRTP()
			if err != nil {
				errCh <- err
				return
			}
			sb.Push(pkt)
		}
	}()

	// Wait for the best keyframe with maximum available quality in a limited interval
	// Save the largest keyframe by resolution seen during this time
	deadline := time.Now().Add(3 * time.Second)
	var bestKeyframe []byte
	bestW, bestH := 0, 0

loop:
	for {
		// If the timeout has expired - save the best keyframe
		if time.Now().After(deadline) {
			break
		}

		select {
		case err, ok := <-errCh:
			if ok && err != nil {
				return nil, fmt.Errorf("RTP read ended: %v", err)
			}
			break loop
		default:
		}

		sample := sb.Pop()
		if sample == nil {
			time.Sleep(2 * time.Millisecond)
			continue
		}

		if !isVP8Keyframe(sample.Data) {
			continue
		}

		if w, h, ok := vp8KeyframeWH(sample.Data); ok {
			// Update the best keyframe if the current one is larger by area
			if w*h > bestW*bestH {
				bestKeyframe = sample.Data
				bestW, bestH = w, h
			}
		}
	}
	if bestKeyframe == nil {
		return nil, fmt.Errorf("%s: failed to get VP8 key frame", funcName(1))
	}
	return bestKeyframe, nil
}

func isVP8Keyframe(frame []byte) bool {
	// VP8 frame tag: bit0 == 0 => keyframe
	return len(frame) > 0 && (frame[0]&0x01) == 0 && frame[3] == 0x9d && frame[4] == 0x01 && frame[5] == 0x2a
}

// Parse width/height from VP8 keyframe header (start code 0x9d 0x01 0x2a).
func vp8KeyframeWH(frame []byte) (int, int, bool) {
	if len(frame) < 10 {
		return 0, 0, false
	}
	// 3 bytes frame tag, then 3 bytes start code
	if frame[3] != 0x9d || frame[4] != 0x01 || frame[5] != 0x2a {
		return 0, 0, false
	}
	w := int(binary.LittleEndian.Uint16(frame[6:8]) & 0x3FFF)
	h := int(binary.LittleEndian.Uint16(frame[8:10]) & 0x3FFF)
	return w, h, true
}

// ===== H264 =====

func makeScreenshotH264(track *webrtc.TrackRemote) ([]byte, error) {
	return getKeyFrameH264(track)
}

func getKeyFrameH264(track *webrtc.TrackRemote) ([]byte, error) {
	const clock = 90000
	sb := samplebuilder.New(200, &codecs.H264Packet{}, clock)

	ctx, cancel := context.WithCancel(context.Background())
	defer cancel() // Ensure goroutine is stopped when function exits

	errCh := make(chan error, 1)
	go func() {
		defer func () {
			if r := recover(); r != nil {
				logger.Errorw("gorouting crached", fmt.Errorf("%+v\n%s", r, funcName(2)))
			}
		}()
		defer close(errCh)
		for {
			select {
			case <-ctx.Done():
				return
			default:
			}

			pkt, _, err := track.ReadRTP()
			if err != nil {
				errCh <- err
				return
			}
			sb.Push(pkt)
		}
	}()

	// Wait for keyframe with maximum available quality
	// H.264 streams often send SPS/PPS separately from IDR frames
	// We need to cache SPS/PPS and combine them with IDR when it arrives
	deadline := time.Now().Add(20 * time.Second)
	var cachedSPS []byte
	var cachedPPS []byte
	var bestKeyframe []byte
	samplesChecked := 0
	startTime := time.Now()

loop:
	for {
		// If timeout - return best keyframe seen
		if time.Now().After(deadline) {
			break
		}

		select {
		case err, ok := <-errCh:
			if ok && err != nil {
				return nil, fmt.Errorf("RTP read ended: %v", err)
			}
			break loop
		default:
		}

		sample := sb.Pop()
		if sample == nil {
			time.Sleep(2 * time.Millisecond)
			continue
		}

		samplesChecked++
		nalTypes := extractH264NALs(sample.Data)

		// Log progress every 2 seconds
		elapsed := time.Since(startTime)
		if samplesChecked == 1 || (samplesChecked%100 == 0) || (int(elapsed.Seconds())%2 == 0 && samplesChecked%10 == 0) {
			logger.Debugw("H264 search", "samplesChecked", samplesChecked, "elapsed", elapsed.Seconds())
		}

		// Cache SPS and PPS when we see them
		if sps, ok := nalTypes[7]; ok {
			cachedSPS = sps
			logger.Debugw("Cached SPS", "length", len(cachedSPS))
		}
		if pps, ok := nalTypes[8]; ok {
			cachedPPS = pps
			logger.Debugw("Cached PPS", "length", len(cachedPPS))
		}

		// If we found an IDR frame and have cached SPS/PPS, combine them
		if idr, ok := nalTypes[5]; ok {
			if cachedSPS != nil && cachedPPS != nil {
				// Build complete keyframe: SPS + PPS + IDR
				keyframe := make([]byte, 0, len(cachedSPS)+len(cachedPPS)+len(idr))
				keyframe = append(keyframe, cachedSPS...)
				keyframe = append(keyframe, cachedPPS...)
				keyframe = append(keyframe, idr...)

				bestKeyframe = keyframe

				break loop
			} else {
				logger.Debugw("Found IDR but missing SPS/PPS", "SPS", cachedSPS != nil, "PPS", cachedPPS != nil)
			}
		}
	}

	elapsed := time.Since(startTime)

	if bestKeyframe == nil {
		return nil, fmt.Errorf("%s: failed to get H264 key frame after checking %d samples in %.1fs", funcName(1), samplesChecked, elapsed.Seconds())
	}
	return bestKeyframe, nil
}

func extractH264NALs(frame []byte) map[int][]byte {
	nalUnits := make(map[int][]byte)
	if len(frame) < 4 {
		return nalUnits
	}

	i := 0
	for i < len(frame)-4 {
		// Look for start code: 0x00 0x00 0x00 0x01 or 0x00 0x00 0x01
		if frame[i] == 0x00 && frame[i+1] == 0x00 {
			startCodeLen := 0
			nalStart := i

			if frame[i+2] == 0x01 {
				startCodeLen = 3
			} else if frame[i+2] == 0x00 && i+3 < len(frame) && frame[i+3] == 0x01 {
				startCodeLen = 4
			}

			if startCodeLen > 0 && i+startCodeLen < len(frame) {
				nalType := int(frame[i+startCodeLen] & 0x1F)

				// Find the end of this NAL unit (next start code or end of frame)
				nalEnd := len(frame)
				j := i + startCodeLen + 1
				for j < len(frame)-3 {
					if frame[j] == 0x00 && frame[j+1] == 0x00 {
						if frame[j+2] == 0x01 || (j+3 < len(frame) && frame[j+2] == 0x00 && frame[j+3] == 0x01) {
							nalEnd = j
							break
						}
					}
					j++
				}

				// Extract NAL unit with start code
				nalUnits[nalType] = frame[nalStart:nalEnd]
				i = nalEnd
			} else {
				i++
			}
		} else {
			i++
		}
	}

	return nalUnits
}
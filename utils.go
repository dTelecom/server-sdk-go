package lksdk

import (
	"encoding/json"
	"fmt"
	"runtime"
	"strings"

	"github.com/pion/webrtc/v3"
	"github.com/thoas/go-funk"

	"github.com/livekit/protocol/livekit"
)


// === Proto ===
func ToProtoSessionDescription(sd webrtc.SessionDescription) *livekit.SessionDescription {
	return &livekit.SessionDescription{
		Type: sd.Type.String(),
		Sdp:  sd.SDP,
	}
}

func FromProtoSessionDescription(sd *livekit.SessionDescription) webrtc.SessionDescription {
	var sdType webrtc.SDPType
	switch sd.Type {
	case webrtc.SDPTypeOffer.String():
		sdType = webrtc.SDPTypeOffer
	case webrtc.SDPTypeAnswer.String():
		sdType = webrtc.SDPTypeAnswer
	case webrtc.SDPTypePranswer.String():
		sdType = webrtc.SDPTypePranswer
	case webrtc.SDPTypeRollback.String():
		sdType = webrtc.SDPTypeRollback
	}
	return webrtc.SessionDescription{
		Type: sdType,
		SDP:  sd.Sdp,
	}
}

func ToProtoTrickle(candidateInit webrtc.ICECandidateInit, target livekit.SignalTarget) *livekit.TrickleRequest {
	data, _ := json.Marshal(candidateInit)
	return &livekit.TrickleRequest{
		CandidateInit: string(data),
		Target:        target,
	}
}

func FromProtoTrickle(trickle *livekit.TrickleRequest) webrtc.ICECandidateInit {
	ci := webrtc.ICECandidateInit{}
	json.Unmarshal([]byte(trickle.CandidateInit), &ci)
	return ci
}

func FromProtoIceServers(iceservers []*livekit.ICEServer) []webrtc.ICEServer {
	servers := funk.Map(iceservers, func(server *livekit.ICEServer) webrtc.ICEServer {
		return webrtc.ICEServer{
			URLs:       server.Urls,
			Username:   server.Username,
			Credential: server.Credential,
		}
	})
	return servers.([]webrtc.ICEServer)
}

// === Url ===
func ToHttpURL(url string) string {
	if strings.HasPrefix(url, "ws") {
		return strings.Replace(url, "ws", "http", 1)
	}
	return url
}

func ToWebsocketURL(url string) string {
	if strings.HasPrefix(url, "http") {
		return strings.Replace(url, "http", "ws", 1)
	}
	return url
}

// === Trace ===

func funcName(skip int) string {
	pc, file, line, _ := runtime.Caller(skip)
	return fmt.Sprintf("%s (%s:%d)", runtime.FuncForPC(pc).Name(), shortFileName(file), line)
}

func shortFileName(fileName string) string {
	index := strings.LastIndex(fileName, "/")
	if index != -1 {
		return fileName[index+1:]
	}
	return fileName
}

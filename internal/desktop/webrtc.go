package desktop

import (
	"encoding/json"
	"fmt"
	"log"
	"net"
	"sync"

	"github.com/pion/webrtc/v4"
)

type WebRTCServer struct {
	peerConnection *webrtc.PeerConnection
	videoTrack     *webrtc.TrackLocalStaticRTP
	udpListener    net.PacketConn
	inputInjector  *XdotoolInjector
	mu             sync.Mutex
	isClosed       bool
}

func NewWebRTCServer(injector *XdotoolInjector) (*WebRTCServer, error) {
	return &WebRTCServer{
		inputInjector: injector,
	}, nil
}

// InitPeerConnection sets up the Pion PeerConnection and local H.264 video track
func (w *WebRTCServer) InitPeerConnection(iceServers []string) error {
	w.mu.Lock()
	defer w.mu.Unlock()

	var webrtcIceServers []webrtc.ICEServer
	for _, url := range iceServers {
		webrtcIceServers = append(webrtcIceServers, webrtc.ICEServer{
			URLs: []string{url},
		})
	}
	if len(webrtcIceServers) == 0 {
		// Default STUN server
		webrtcIceServers = append(webrtcIceServers, webrtc.ICEServer{
			URLs: []string{"stun:stun.l.google.com:19302"},
		})
	}

	config := webrtc.Configuration{
		ICEServers: webrtcIceServers,
	}

	pc, err := webrtc.NewPeerConnection(config)
	if err != nil {
		return fmt.Errorf("failed to create WebRTC PeerConnection: %w", err)
	}

	// Create H.264 video track
	videoTrack, err := webrtc.NewTrackLocalStaticRTP(
		webrtc.RTPCodecCapability{MimeType: webrtc.MimeTypeH264},
		"video",
		"xworkmate-desktop",
	)
	if err != nil {
		_ = pc.Close()
		return fmt.Errorf("failed to create video track: %w", err)
	}

	_, err = pc.AddTrack(videoTrack)
	if err != nil {
		_ = pc.Close()
		return fmt.Errorf("failed to add video track: %w", err)
	}

	// Handle Data Channel for inputs
	pc.OnDataChannel(func(d *webrtc.DataChannel) {
		log.Printf("Data channel '%s'-'%d' opened", d.Label(), d.ID())
		if d.Label() == "input" {
			d.OnMessage(func(msg webrtc.DataChannelMessage) {
				var event InputEvent
				if err := json.Unmarshal(msg.Data, &event); err != nil {
					log.Printf("Failed to unmarshal input event: %v", err)
					return
				}
				if err := w.inputInjector.Inject(event); err != nil {
					log.Printf("Failed to inject input event: %v", err)
				}
			})
		}
	})

	w.peerConnection = pc
	w.videoTrack = videoTrack

	return nil
}

// StartRTPReceiver listens on local UDP port for GStreamer RTP stream and forwards to WebRTC video track
func (w *WebRTCServer) StartRTPReceiver(port int) error {
	addr := fmt.Sprintf("127.0.0.1:%d", port)
	conn, err := net.ListenPacket("udp", addr)
	if err != nil {
		return fmt.Errorf("failed to bind UDP port %s: %w", addr, err)
	}

	w.mu.Lock()
	w.udpListener = conn
	w.mu.Unlock()

	go func() {
		buf := make([]byte, 2048)
		log.Printf("WebRTC RTP receiver listening on UDP %s", addr)
		for {
			n, _, err := conn.ReadFrom(buf)
			if err != nil {
				w.mu.Lock()
				closed := w.isClosed
				w.mu.Unlock()
				if !closed {
					log.Printf("UDP RTP read error: %v", err)
				}
				break
			}

			// Forward packet directly to WebRTC track (zero-copy)
			w.mu.Lock()
			track := w.videoTrack
			w.mu.Unlock()

			if track != nil {
				if _, err := track.Write(buf[:n]); err != nil {
					log.Printf("Failed to write RTP packet to track: %v", err)
				}
			}
		}
	}()

	return nil
}

// ProcessOffer handles SDP offer, sets remote description, generates and returns SDP answer
func (w *WebRTCServer) ProcessOffer(sdpOffer string) (string, error) {
	w.mu.Lock()
	pc := w.peerConnection
	w.mu.Unlock()

	if pc == nil {
		return "", fmt.Errorf("peer connection not initialized")
	}

	offer := webrtc.SessionDescription{
		Type: webrtc.SDPTypeOffer,
		SDP:  sdpOffer,
	}

	if err := pc.SetRemoteDescription(offer); err != nil {
		return "", fmt.Errorf("failed to set remote description: %w", err)
	}

	answer, err := pc.CreateAnswer(nil)
	if err != nil {
		return "", fmt.Errorf("failed to create SDP answer: %w", err)
	}

	// Gather ICE candidates
	gatherComplete := webrtc.GatheringCompletePromise(pc)

	if err := pc.SetLocalDescription(answer); err != nil {
		return "", fmt.Errorf("failed to set local description: %w", err)
	}

	<-gatherComplete

	localDesc := pc.LocalDescription()
	if localDesc == nil {
		return "", fmt.Errorf("local description is nil after gathering")
	}

	return localDesc.SDP, nil
}

// AddICECandidate adds a remote ICE candidate
func (w *WebRTCServer) AddICECandidate(candidate webrtc.ICECandidateInit) error {
	w.mu.Lock()
	pc := w.peerConnection
	w.mu.Unlock()

	if pc == nil {
		return fmt.Errorf("peer connection not initialized")
	}

	if err := pc.AddICECandidate(candidate); err != nil {
		return fmt.Errorf("failed to add remote ICE candidate: %w", err)
	}

	return nil
}

// Close terminates the WebRTC server
func (w *WebRTCServer) Close() {
	w.mu.Lock()
	if w.isClosed {
		w.mu.Unlock()
		return
	}
	w.isClosed = true
	pc := w.peerConnection
	conn := w.udpListener
	w.mu.Unlock()

	log.Println("Closing WebRTC server...")

	if conn != nil {
		_ = conn.Close()
	}

	if pc != nil {
		_ = pc.Close()
	}
}

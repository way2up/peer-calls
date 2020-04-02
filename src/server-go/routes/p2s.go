package routes

import (
	"fmt"
	"net/http"
	"reflect"
	"unsafe"

	"github.com/jeremija/peer-calls/src/server-go/config"
	"github.com/jeremija/peer-calls/src/server-go/routes/wsserver"
	"github.com/jeremija/peer-calls/src/server-go/wrtc"
	"github.com/jeremija/peer-calls/src/server-go/wrtc/tracks"
	"github.com/jeremija/peer-calls/src/server-go/ws/wsmessage"
	"github.com/pion/webrtc/v2"
)

const localPeerID = "__SERVER__"

type TracksManager interface {
	Add(room string, clientID string, peerConnection tracks.PeerConnection, signaller tracks.Signaller)
}

const serverIsInitiator = false

func NewPeerToServerRoomHandler(
	wss *wsserver.WSS,
	iceServers []config.ICEServer,
	tracksManager TracksManager,
) http.Handler {

	webrtcICEServers := []webrtc.ICEServer{}
	for _, iceServer := range iceServers {
		var c webrtc.ICECredentialType
		if iceServer.AuthType == config.AuthTypeSecret {
			c = webrtc.ICECredentialTypePassword
		}
		webrtcICEServers = append(webrtcICEServers, webrtc.ICEServer{
			URLs:           iceServer.URLs,
			CredentialType: c,
			Username:       iceServer.AuthSecret.Username,
			Credential:     iceServer.AuthSecret.Secret,
		})
	}

	webrtcConfig := webrtc.Configuration{
		ICEServers: webrtcICEServers,
	}
	mediaEngine := webrtc.MediaEngine{}

	fn := func(w http.ResponseWriter, r *http.Request) {

		// mediaEngine := webrtc.MediaEngine{}
		settingEngine := webrtc.SettingEngine{}
		// settingEngine.SetTrickle(true)
		api := webrtc.NewAPI(
			webrtc.WithMediaEngine(mediaEngine),
			webrtc.WithSettingEngine(settingEngine),
		)

		// Hack to be able to update dynamic codec payload IDs with every new sdp
		// renegotiation of passive (non-server initiated) peer connections.
		field := reflect.ValueOf(api).Elem().FieldByName("mediaEngine")
		unsafeField := reflect.NewAt(field.Type(), unsafe.Pointer(field.UnsafeAddr())).Elem()

		mediaEngine, ok := unsafeField.Interface().(*webrtc.MediaEngine)
		if !ok {
			log.Printf("Error in hack to obtain mediaEngine")
			w.WriteHeader(http.StatusInternalServerError)
			return
		}
		peerConnection, err := api.NewPeerConnection(webrtcConfig)

		if err != nil {
			log.Printf("Error creating peer connection: %s", err)
			w.WriteHeader(http.StatusInternalServerError)
			return
		}

		cleanup := func() {
			// TODO maybe cleanup is not necessary as we can still keep peer
			// connections after websocket conn closes
		}

		var signaller *wrtc.Signaller

		peerConnection.OnICEGatheringStateChange(func(state webrtc.ICEGathererState) {
			log.Printf("ICE gathering state changed: %s", state)
		})

		handleMessage := func(event wsserver.RoomEvent) {
			msg := event.Message
			adapter := event.Adapter
			room := event.Room
			clientID := event.ClientID

			initiator := localPeerID
			if !serverIsInitiator {
				initiator = clientID
			}

			var responseEventName string
			var err error

			switch msg.Type {
			case "ready":
				log.Printf("Initiator for clientID: %s is: %s", clientID, initiator)

				responseEventName = "users"
				err = adapter.Broadcast(
					wsmessage.NewMessage(responseEventName, room, map[string]interface{}{
						"initiator": initiator,
						// "initiator": clientID,
						"users": []User{{UserID: localPeerID, ClientID: localPeerID}},
					}),
				)

				if initiator == localPeerID {
					// need to do this to connect with simple peer
					// only when we are the initiator
					_, err = peerConnection.CreateDataChannel("test", nil)
					if err != nil {
						log.Printf("Error creating data channel")
						// TODO abort connection
					}
				}

				// TODO use this to get all client IDs and request all tracks of all users
				// adapter.Clients()
				if signaller == nil {
					signaller, err = wrtc.NewSignaller(
						initiator == localPeerID,
						peerConnection,
						mediaEngine,
						localPeerID,
						func(signal interface{}) {
							log.Printf("Sending local signal to remote clientID: %s", clientID)
							err := adapter.Emit(clientID, wsmessage.NewMessage("signal", room, signal))
							if err != nil {
								log.Printf("Error sending local signal to remote clientID: %s: %s", clientID, err)
								// TODO abort connection
							}
						},
					)
					if err != nil {
						err = fmt.Errorf("Error initializing signaller: %s", err)
						break
					}
					tracksManager.Add(room, clientID, peerConnection, signaller)
				}
			case "signal":
				payload, _ := msg.Payload.(map[string]interface{})
				if signaller == nil {
					err = fmt.Errorf("Ignoring signal because signaller is not initialized")
				} else {
					err = signaller.Signal(payload)
				}
			}

			if err != nil {
				log.Printf("Error handling event (event: %s, room: %s, source: %s): %s", msg.Type, room, clientID, err)
			}
		}

		wss.HandleRoomWithCleanup(w, r, handleMessage, cleanup)
	}
	return http.HandlerFunc(fn)
}

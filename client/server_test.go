package client

import (
	"testing"

	"github.com/andree-bjorkgard/remote-bluetooth/v2/bluez"
	pb "github.com/andree-bjorkgard/remote-bluetooth/v2/internal/pb/remotebtv1"
)

func TestFromProtoAudioAndReleased(t *testing.T) {
	tests := []struct {
		name     string
		in       *pb.Device
		audio    bluez.AudioState
		released bool
	}{
		{"none", &pb.Device{}, bluez.AudioNone, false},
		{"idle", &pb.Device{AudioState: pb.AudioState_AUDIO_STATE_IDLE}, bluez.AudioIdle, false},
		{"sending", &pb.Device{AudioState: pb.AudioState_AUDIO_STATE_SENDING}, bluez.AudioSending, false},
		{"released", &pb.Device{Released: true}, bluez.AudioNone, true},
		{"unknown enum", &pb.Device{AudioState: pb.AudioState(99)}, bluez.AudioNone, false},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			d := fromProto("srv", tt.in)
			if d.Audio != tt.audio || d.Released != tt.released || d.Server != "srv" {
				t.Errorf("got %+v, want audio %v released %v", d, tt.audio, tt.released)
			}
		})
	}
}

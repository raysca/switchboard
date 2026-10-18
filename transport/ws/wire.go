package ws

import (
	"errors"
	"fmt"
	"time"

	"google.golang.org/protobuf/proto"

	"govoice/core"
	framespb "govoice/core/transport/v1/framespb"
)

const (
	MaxEncodedFrameSize   = 20 * 1024 // the largest valid frame is a 16 KB message plus a few bytes (audio is at most 4 KB)
	MaxAudioBytes         = 4096      // Max 4 KB raw PCM16 per 20-40ms chunk
	MaxMessageDataBytes   = 16 * 1024 // Max 16 KB nested message/RTVI JSON string
	MinChunkDuration      = 10 * time.Millisecond
	MaxChunkDuration      = 40 * time.Millisecond
	InternalSampleRate    = 16000 // 16 kHz internal STT/VAD format
	FixedPlayerSampleRate = 24000 // 24 kHz fixed client player format
)

var (
	ErrOversizedMessage     = errors.New("ws: encoded message exceeds size limit")
	ErrOversizedAudio       = errors.New("ws: decoded audio payload exceeds size limit")
	ErrOversizedMessageData = errors.New("ws: decoded message data exceeds size limit")
	ErrMalformedProtobuf    = errors.New("ws: malformed protobuf frame")
	ErrUnknownVariant       = errors.New("ws: unknown or unsupported protobuf frame variant")
	ErrInvalidPCMFormat     = errors.New("ws: invalid PCM audio format (must be 16-bit mono)")
	ErrUnalignedPCM         = errors.New("ws: PCM audio bytes not 2-byte sample aligned")
	ErrUnsupportedRate      = errors.New("ws: unsupported input sample rate")
	ErrInvalidDuration      = errors.New("ws: audio chunk duration outside allowed 10-40ms bounds")
)

// FrameWireMessage is a custom frame type for raw wire messages (e.g. RTVI JSON).
const FrameWireMessage core.FrameType = 850

// WireMessageFrame carries non-audio messages received over the WebSocket.
type WireMessageFrame struct {
	Data      string
	Seq       uint64
	Timestamp time.Time
}

func (f *WireMessageFrame) Type() core.FrameType { return FrameWireMessage }

// DecodeBinaryFrame validates and decodes a single binary WebSocket message into a core frame.
func DecodeBinaryFrame(raw []byte, seq uint64, now time.Time) (core.Frame, error) {
	if len(raw) > MaxEncodedFrameSize {
		return nil, ErrOversizedMessage
	}

	var pbFrame framespb.Frame
	if err := proto.Unmarshal(raw, &pbFrame); err != nil {
		return nil, fmt.Errorf("%w: %v", ErrMalformedProtobuf, err)
	}

	switch variant := pbFrame.Frame.(type) {
	case *framespb.Frame_Audio:
		audio := variant.Audio
		if audio == nil {
			return nil, fmt.Errorf("%w: nil audio payload", ErrMalformedProtobuf)
		}
		if len(audio.Audio) > MaxAudioBytes {
			return nil, ErrOversizedAudio
		}
		if len(audio.Audio)%2 != 0 {
			return nil, ErrUnalignedPCM
		}
		if audio.NumChannels != 1 {
			return nil, fmt.Errorf("%w: channels must be 1, got %d", ErrInvalidPCMFormat, audio.NumChannels)
		}
		rate := int(audio.SampleRate)
		if rate != 16000 && rate != 24000 && rate != 48000 {
			return nil, fmt.Errorf("%w: got %d Hz", ErrUnsupportedRate, rate)
		}

		samples := len(audio.Audio) / 2
		duration := time.Duration(samples) * time.Second / time.Duration(rate)
		if duration < MinChunkDuration || duration > MaxChunkDuration {
			return nil, fmt.Errorf("%w: got %v", ErrInvalidDuration, duration)
		}

		return &core.InputAudioFrame{
			Samples:    audio.Audio,
			Encoding:   core.EncodingPCM16LE,
			SampleRate: rate,
			Channels:   1,
			Duration:   duration,
			Timestamp:  now,
			Seq:        seq,
		}, nil

	case *framespb.Frame_Message:
		msg := variant.Message
		if msg == nil {
			return nil, fmt.Errorf("%w: nil message payload", ErrMalformedProtobuf)
		}
		if len(msg.Data) > MaxMessageDataBytes {
			return nil, ErrOversizedMessageData
		}
		return &WireMessageFrame{
			Data:      msg.Data,
			Seq:       seq,
			Timestamp: now,
		}, nil

	default:
		return nil, ErrUnknownVariant
	}
}

// EncodeAudioFrame packages raw PCM16LE mono audio samples into a binary protobuf frame.
func EncodeAudioFrame(samples []byte, rate int, channels int) ([]byte, error) {
	if len(samples)%2 != 0 {
		return nil, ErrUnalignedPCM
	}
	if channels != 1 {
		return nil, ErrInvalidPCMFormat
	}
	pbFrame := &framespb.Frame{
		Frame: &framespb.Frame_Audio{
			Audio: &framespb.AudioRawFrame{
				Audio:       samples,
				SampleRate:  uint32(rate),
				NumChannels: uint32(channels),
			},
		},
	}
	return proto.Marshal(pbFrame)
}

// EncodeMessageFrame packages a string/RTVI JSON payload into a binary protobuf frame.
func EncodeMessageFrame(data string) ([]byte, error) {
	if len(data) > MaxMessageDataBytes {
		return nil, ErrOversizedMessageData
	}
	pbFrame := &framespb.Frame{
		Frame: &framespb.Frame_Message{
			Message: &framespb.MessageFrame{
				Data: data,
			},
		},
	}
	return proto.Marshal(pbFrame)
}

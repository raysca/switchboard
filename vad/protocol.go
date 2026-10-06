package vad

import (
	"encoding/binary"
	"errors"
	"fmt"
	"io"
	"math"
)

const (
	// IPC Magic bytes: "VAD1" (0x56414431)
	MagicBytes uint32 = 0x56414431

	// ProtocolVersion is the fixed IPC protocol version.
	ProtocolVersion uint16 = 1

	// MaxMessageLength bounds allocation to 64KB before read.
	MaxMessageLength uint32 = 64 * 1024

	// AudioFormatPCM16kFloat32 identifies 16000 Hz float32 mono audio (576 samples).
	AudioFormatPCM16kFloat32 uint32 = 1

	// Message Types
	MsgTypeHandshakeReq  uint16 = 1
	MsgTypeHandshakeResp uint16 = 2
	MsgTypeInferReq      uint16 = 3
	MsgTypeInferResp     uint16 = 4
	MsgTypeFaultInject   uint16 = 5

	// Status Codes
	StatusOK                uint32 = 0
	StatusError             uint32 = 1
	StatusTimeout           uint32 = 2
	StatusInvalidPayload    uint32 = 3
	StatusModelMismatch     uint32 = 4
	StatusVersionMismatch   uint32 = 5
	StatusWorkerUnavailable uint32 = 6
)

var (
	ErrInvalidMagic      = errors.New("vad ipc: invalid magic bytes")
	ErrInvalidVersion    = errors.New("vad ipc: protocol version mismatch")
	ErrInvalidFrameSize  = errors.New("vad ipc: invalid frame size")
	ErrInvalidMsgType    = errors.New("vad ipc: invalid message type")
	ErrModelHashMismatch = errors.New("vad ipc: model SHA-256 hash mismatch")
	ErrWorkerHandshake   = errors.New("vad ipc: worker handshake failed")
	ErrWorkerHung        = errors.New("vad ipc: worker inference exceeded parent deadline")
)

// HandshakeReq is sent by parent to worker upon launch.
type HandshakeReq struct {
	ExpectedModelSHA256 [32]byte
	MaxBatch            uint32
}

// HandshakeResp is returned by worker to parent.
type HandshakeResp struct {
	Status            uint32
	WorkerPID         uint32
	ActualModelSHA256 [32]byte
	ORTVersion        [32]byte
}

// InferItemReq is one inference unit within an InferReq.
type InferItemReq struct {
	RequestID         uint64
	SessionID         uint64
	Seq               uint64
	RelativeTimeoutMs uint32
	AudioFormat       uint32
	State             [StateLen]float32
	Input             [InputSamples]float32
}

// InferItemResp is one inference result within an InferResp.
type InferItemResp struct {
	RequestID uint64
	SessionID uint64
	Seq       uint64
	Status    uint32
	Prob      float32
	State     [StateLen]float32
}

// ReadFrame reads a length-prefixed frame from r without allocating beyond MaxMessageLength.
func ReadFrame(r io.Reader, buf []byte) ([]byte, error) {
	var lenBuf [4]byte
	if _, err := io.ReadFull(r, lenBuf[:]); err != nil {
		return nil, err
	}
	length := binary.LittleEndian.Uint32(lenBuf[:])
	if length == 0 || length > MaxMessageLength {
		return nil, fmt.Errorf("%w: length %d (max %d)", ErrInvalidFrameSize, length, MaxMessageLength)
	}

	payload := buf
	if uint32(cap(payload)) < length {
		payload = make([]byte, length)
	} else {
		payload = payload[:length]
	}

	if _, err := io.ReadFull(r, payload); err != nil {
		return nil, err
	}
	return payload, nil
}

// WriteFrame writes a length-prefixed frame to w.
func WriteFrame(w io.Writer, payload []byte) error {
	length := uint32(len(payload))
	if length == 0 || length > MaxMessageLength {
		return fmt.Errorf("%w: length %d", ErrInvalidFrameSize, length)
	}
	var lenBuf [4]byte
	binary.LittleEndian.PutUint32(lenBuf[:], length)
	if _, err := w.Write(lenBuf[:]); err != nil {
		return err
	}
	_, err := w.Write(payload)
	return err
}

// EncodeHandshakeReq encodes HandshakeReq into payload.
func EncodeHandshakeReq(req HandshakeReq) []byte {
	buf := make([]byte, 4+2+2+32+4)
	binary.LittleEndian.PutUint32(buf[0:4], MagicBytes)
	binary.LittleEndian.PutUint16(buf[4:6], ProtocolVersion)
	binary.LittleEndian.PutUint16(buf[6:8], MsgTypeHandshakeReq)
	copy(buf[8:40], req.ExpectedModelSHA256[:])
	binary.LittleEndian.PutUint32(buf[40:44], req.MaxBatch)
	return buf
}

// DecodeHandshakeReq decodes HandshakeReq from payload.
func DecodeHandshakeReq(data []byte) (HandshakeReq, error) {
	if len(data) < 44 {
		return HandshakeReq{}, ErrInvalidFrameSize
	}
	magic := binary.LittleEndian.Uint32(data[0:4])
	if magic != MagicBytes {
		return HandshakeReq{}, ErrInvalidMagic
	}
	version := binary.LittleEndian.Uint16(data[4:6])
	if version != ProtocolVersion {
		return HandshakeReq{}, ErrInvalidVersion
	}
	msgType := binary.LittleEndian.Uint16(data[6:8])
	if msgType != MsgTypeHandshakeReq {
		return HandshakeReq{}, ErrInvalidMsgType
	}
	var req HandshakeReq
	copy(req.ExpectedModelSHA256[:], data[8:40])
	req.MaxBatch = binary.LittleEndian.Uint32(data[40:44])
	return req, nil
}

// EncodeHandshakeResp encodes HandshakeResp into payload.
func EncodeHandshakeResp(resp HandshakeResp) []byte {
	buf := make([]byte, 4+2+2+4+4+32+32)
	binary.LittleEndian.PutUint32(buf[0:4], MagicBytes)
	binary.LittleEndian.PutUint16(buf[4:6], ProtocolVersion)
	binary.LittleEndian.PutUint16(buf[6:8], MsgTypeHandshakeResp)
	binary.LittleEndian.PutUint32(buf[8:12], resp.Status)
	binary.LittleEndian.PutUint32(buf[12:16], resp.WorkerPID)
	copy(buf[16:48], resp.ActualModelSHA256[:])
	copy(buf[48:80], resp.ORTVersion[:])
	return buf
}

// DecodeHandshakeResp decodes HandshakeResp from payload.
func DecodeHandshakeResp(data []byte) (HandshakeResp, error) {
	if len(data) < 80 {
		return HandshakeResp{}, ErrInvalidFrameSize
	}
	magic := binary.LittleEndian.Uint32(data[0:4])
	if magic != MagicBytes {
		return HandshakeResp{}, ErrInvalidMagic
	}
	version := binary.LittleEndian.Uint16(data[4:6])
	if version != ProtocolVersion {
		return HandshakeResp{}, ErrInvalidVersion
	}
	msgType := binary.LittleEndian.Uint16(data[6:8])
	if msgType != MsgTypeHandshakeResp {
		return HandshakeResp{}, ErrInvalidMsgType
	}
	var resp HandshakeResp
	resp.Status = binary.LittleEndian.Uint32(data[8:12])
	resp.WorkerPID = binary.LittleEndian.Uint32(data[12:16])
	copy(resp.ActualModelSHA256[:], data[16:48])
	copy(resp.ORTVersion[:], data[48:80])
	return resp, nil
}

const inferItemReqBytes = 8 + 8 + 8 + 4 + 4 + StateLen*4 + InputSamples*4 // 32 + 1024 + 2304 = 3360 bytes
const inferItemRespBytes = 8 + 8 + 8 + 4 + 4 + StateLen*4                 // 32 + 1024 = 1056 bytes

// EncodeInferReq encodes an InferReq batch into payload bytes.
func EncodeInferReq(items []InferItemReq) []byte {
	batchSize := uint32(len(items))
	totalSize := 4 + 2 + 2 + 4 + int(batchSize)*inferItemReqBytes
	buf := make([]byte, totalSize)
	binary.LittleEndian.PutUint32(buf[0:4], MagicBytes)
	binary.LittleEndian.PutUint16(buf[4:6], ProtocolVersion)
	binary.LittleEndian.PutUint16(buf[6:8], MsgTypeInferReq)
	binary.LittleEndian.PutUint32(buf[8:12], batchSize)

	off := 12
	for i := 0; i < int(batchSize); i++ {
		item := &items[i]
		binary.LittleEndian.PutUint64(buf[off:off+8], item.RequestID)
		binary.LittleEndian.PutUint64(buf[off+8:off+16], item.SessionID)
		binary.LittleEndian.PutUint64(buf[off+16:off+24], item.Seq)
		binary.LittleEndian.PutUint32(buf[off+24:off+28], item.RelativeTimeoutMs)
		binary.LittleEndian.PutUint32(buf[off+28:off+32], item.AudioFormat)
		off += 32

		for s := 0; s < StateLen; s++ {
			binary.LittleEndian.PutUint32(buf[off:off+4], math.Float32bits(item.State[s]))
			off += 4
		}
		for in := 0; in < InputSamples; in++ {
			binary.LittleEndian.PutUint32(buf[off:off+4], math.Float32bits(item.Input[in]))
			off += 4
		}
	}
	return buf
}

// DecodeInferReq decodes an InferReq batch from payload bytes.
func DecodeInferReq(data []byte) ([]InferItemReq, error) {
	if len(data) < 12 {
		return nil, ErrInvalidFrameSize
	}
	magic := binary.LittleEndian.Uint32(data[0:4])
	if magic != MagicBytes {
		return nil, ErrInvalidMagic
	}
	version := binary.LittleEndian.Uint16(data[4:6])
	if version != ProtocolVersion {
		return nil, ErrInvalidVersion
	}
	msgType := binary.LittleEndian.Uint16(data[6:8])
	if msgType != MsgTypeInferReq {
		return nil, ErrInvalidMsgType
	}
	batchSize := binary.LittleEndian.Uint32(data[8:12])
	expectedLen := 12 + int(batchSize)*inferItemReqBytes
	if len(data) < expectedLen {
		return nil, ErrInvalidFrameSize
	}

	items := make([]InferItemReq, batchSize)
	off := 12
	for i := 0; i < int(batchSize); i++ {
		item := &items[i]
		item.RequestID = binary.LittleEndian.Uint64(data[off : off+8])
		item.SessionID = binary.LittleEndian.Uint64(data[off+8 : off+16])
		item.Seq = binary.LittleEndian.Uint64(data[off+16 : off+24])
		item.RelativeTimeoutMs = binary.LittleEndian.Uint32(data[off+24 : off+28])
		item.AudioFormat = binary.LittleEndian.Uint32(data[off+28 : off+32])
		off += 32

		for s := 0; s < StateLen; s++ {
			item.State[s] = math.Float32frombits(binary.LittleEndian.Uint32(data[off : off+4]))
			off += 4
		}
		for in := 0; in < InputSamples; in++ {
			item.Input[in] = math.Float32frombits(binary.LittleEndian.Uint32(data[off : off+4]))
			off += 4
		}
	}
	return items, nil
}

// EncodeInferResp encodes an InferResp batch into payload bytes.
func EncodeInferResp(items []InferItemResp) []byte {
	batchSize := uint32(len(items))
	totalSize := 4 + 2 + 2 + 4 + int(batchSize)*inferItemRespBytes
	buf := make([]byte, totalSize)
	binary.LittleEndian.PutUint32(buf[0:4], MagicBytes)
	binary.LittleEndian.PutUint16(buf[4:6], ProtocolVersion)
	binary.LittleEndian.PutUint16(buf[6:8], MsgTypeInferResp)
	binary.LittleEndian.PutUint32(buf[8:12], batchSize)

	off := 12
	for i := 0; i < int(batchSize); i++ {
		item := &items[i]
		binary.LittleEndian.PutUint64(buf[off:off+8], item.RequestID)
		binary.LittleEndian.PutUint64(buf[off+8:off+16], item.SessionID)
		binary.LittleEndian.PutUint64(buf[off+16:off+24], item.Seq)
		binary.LittleEndian.PutUint32(buf[off+24:off+28], item.Status)
		binary.LittleEndian.PutUint32(buf[off+28:off+32], math.Float32bits(item.Prob))
		off += 32

		for s := 0; s < StateLen; s++ {
			binary.LittleEndian.PutUint32(buf[off:off+4], math.Float32bits(item.State[s]))
			off += 4
		}
	}
	return buf
}

// DecodeInferResp decodes an InferResp batch from payload bytes.
func DecodeInferResp(data []byte) ([]InferItemResp, error) {
	if len(data) < 12 {
		return nil, ErrInvalidFrameSize
	}
	magic := binary.LittleEndian.Uint32(data[0:4])
	if magic != MagicBytes {
		return nil, ErrInvalidMagic
	}
	version := binary.LittleEndian.Uint16(data[4:6])
	if version != ProtocolVersion {
		return nil, ErrInvalidVersion
	}
	msgType := binary.LittleEndian.Uint16(data[6:8])
	if msgType != MsgTypeInferResp {
		return nil, ErrInvalidMsgType
	}
	batchSize := binary.LittleEndian.Uint32(data[8:12])
	expectedLen := 12 + int(batchSize)*inferItemRespBytes
	if len(data) < expectedLen {
		return nil, ErrInvalidFrameSize
	}

	items := make([]InferItemResp, batchSize)
	off := 12
	for i := 0; i < int(batchSize); i++ {
		item := &items[i]
		item.RequestID = binary.LittleEndian.Uint64(data[off : off+8])
		item.SessionID = binary.LittleEndian.Uint64(data[off+8 : off+16])
		item.Seq = binary.LittleEndian.Uint64(data[off+16 : off+24])
		item.Status = binary.LittleEndian.Uint32(data[off+24 : off+28])
		item.Prob = math.Float32frombits(binary.LittleEndian.Uint32(data[off+28 : off+32]))
		off += 32

		for s := 0; s < StateLen; s++ {
			item.State[s] = math.Float32frombits(binary.LittleEndian.Uint32(data[off : off+4]))
			off += 4
		}
	}
	return items, nil
}

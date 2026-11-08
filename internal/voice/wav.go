package voice

import (
	"encoding/binary"
	"io"
)

// WriteWAV writes mono 16-bit PCM WAV.
func WriteWAV(w io.Writer, a Audio) error {
	data := FloatToPCM16(a.Samples)
	hdr := struct {
		RIFF          [4]byte
		Size          uint32
		WAVE, Fmt     [4]byte
		FmtSize       uint32
		Format, Chans uint16
		Rate, ByteRt  uint32
		Align, Bits   uint16
		Data          [4]byte
		DataSize      uint32
	}{
		[4]byte{'R', 'I', 'F', 'F'}, uint32(36 + len(data)),
		[4]byte{'W', 'A', 'V', 'E'}, [4]byte{'f', 'm', 't', ' '},
		16, 1, 1, uint32(a.SampleRate), uint32(a.SampleRate * 2), 2, 16,
		[4]byte{'d', 'a', 't', 'a'}, uint32(len(data)),
	}
	if err := binary.Write(w, binary.LittleEndian, hdr); err != nil {
		return err
	}
	_, err := w.Write(data)
	return err
}

package wav

import (
	"bytes"
	"encoding/binary"
	"errors"
	"fmt"
	"io"
	"os"
	"sync"
	"time"

	"github.com/go-audio/audio"
	"github.com/go-audio/riff"
)

type WriterAtSeeker interface {
	io.Writer
	io.WriterAt
	io.Seeker
}

// Encoder encodes LPCM data into a wav containter.
type Encoder struct {
	mu      sync.Mutex
	w       WriterAtSeeker
	bufPool *sync.Pool

	SampleRate int
	BitDepth   int
	NumChans   int

	// A number indicating the WAVE format category of the file. The content of
	// the <format-specific-fields> portion of the ‘fmt’ chunk, and the
	// interpretation of the waveform data, depend on this value. PCM = 1 (i.e.
	// Linear quantization) Values other than 1 indicate some form of
	// compression.
	WavAudioFormat int

	// Metadata contains metadata to inject in the file.
	Metadata *Metadata

	WrittenBytes    int
	frames          int
	pcmChunkStarted bool
	pcmChunkSizePos int
	pcmChunkPos     int64
	wroteHeader     bool // true if we've written the header out
}

// NewEncoder creates a new encoder to create a new wav file.
// Don't forget to add Frames to the encoder before writing.
func NewEncoder(w WriterAtSeeker, sampleRate, bitDepth, numChans, audioFormat int) *Encoder {
	return &Encoder{
		w: w,
		bufPool: &sync.Pool{New: func() interface{} {
			return bytes.NewBuffer(make([]byte, 0, bytesNumFromDuration(time.Minute, sampleRate, bitDepth)*numChans))
		}},
		SampleRate:     sampleRate,
		BitDepth:       bitDepth,
		NumChans:       numChans,
		WavAudioFormat: audioFormat,
	}
}

// AddLE serializes and adds the passed value using little endian
func (e *Encoder) AddLE(src interface{}) error {
	e.WrittenBytes += binary.Size(src)
	return binary.Write(e.w, binary.LittleEndian, src)
}

// AddBE serializes and adds the passed value using big endian
func (e *Encoder) AddBE(src interface{}) error {
	e.WrittenBytes += binary.Size(src)
	return binary.Write(e.w, binary.BigEndian, src)
}

func (e *Encoder) addBuffer(buf *audio.IntBuffer, pos *int64) (int64, error) {
	if buf == nil {
		return 0, fmt.Errorf("can't add a nil buffer")
	}

	binaryBuf := e.bufPool.Get().(*bytes.Buffer)
	defer e.bufPool.Put(binaryBuf)

	frameCount := buf.NumFrames()
	// performance tweak: setup a buffer so we don't do too many writes
	var err error

	bufferFrames := 0
	for i := 0; i < frameCount; i++ {
		for j := 0; j < buf.Format.NumChannels; j++ {
			v := buf.Data[i*buf.Format.NumChannels+j]
			switch e.BitDepth {
			case 8:
				if err = binary.Write(binaryBuf, binary.LittleEndian, uint8(v)); err != nil {
					return 0, err
				}
			case 16:
				if err = binary.Write(binaryBuf, binary.LittleEndian, int16(v)); err != nil {
					return 0, err
				}
			case 24:
				if err = binary.Write(binaryBuf, binary.LittleEndian, audio.Int32toInt24LEBytes(int32(v))); err != nil {
					return 0, err
				}
			case 32:
				if err = binary.Write(binaryBuf, binary.LittleEndian, int32(v)); err != nil {
					return 0, err
				}
			default:
				return 0, fmt.Errorf("can't add frames of bit size %d", e.BitDepth)
			}
		}
		bufferFrames++
	}

	var n int
	if pos == nil {
		n, err = e.w.Write(binaryBuf.Bytes())
	} else {
		n, err = e.w.WriteAt(binaryBuf.Bytes(), e.pcmChunkPos+*pos)
	}

	e.mu.Lock()
	e.frames += bufferFrames
	e.WrittenBytes += n
	e.mu.Unlock()
	binaryBuf.Reset()

	return int64(n), nil
}

func (e *Encoder) writeHeader() error {
	if e.wroteHeader {
		return errors.New("already wrote header")
	}
	e.wroteHeader = true
	if e == nil {
		return fmt.Errorf("can't write a nil encoder")
	}
	if e.w == nil {
		return fmt.Errorf("can't write to a nil writer")
	}

	if e.WrittenBytes > 0 {
		return nil
	}

	// riff ID
	if err := e.AddLE(riff.RiffID); err != nil {
		return err
	}
	// file size uint32, to update later on.
	if err := e.AddLE(uint32(42)); err != nil {
		return err
	}
	// wave headers
	if err := e.AddLE(riff.WavFormatID); err != nil {
		return err
	}
	// form
	if err := e.AddLE(riff.FmtID); err != nil {
		return err
	}
	// chunk size
	if err := e.AddLE(uint32(16)); err != nil {
		return err
	}
	// wave format
	if err := e.AddLE(uint16(e.WavAudioFormat)); err != nil {
		return err
	}
	// num channels
	if err := e.AddLE(uint16(e.NumChans)); err != nil {
		return fmt.Errorf("error encoding the number of channels - %w", err)
	}
	// samplerate
	if err := e.AddLE(uint32(e.SampleRate)); err != nil {
		return fmt.Errorf("error encoding the sample rate - %w", err)
	}
	blockAlign := e.NumChans * e.BitDepth / 8
	// avg bytes per sec
	if err := e.AddLE(uint32(e.SampleRate * blockAlign)); err != nil {
		return fmt.Errorf("error encoding the avg bytes per sec - %w", err)
	}
	// block align
	if err := e.AddLE(uint16(blockAlign)); err != nil {
		return err
	}
	// bits per sample
	if err := e.AddLE(uint16(e.BitDepth)); err != nil {
		return fmt.Errorf("error encoding bits per sample - %w", err)
	}

	return nil
}

// Write encodes and writes the passed buffer to the underlying writer.
// Don't forget to Close() the encoder or the file won't be valid.
func (e *Encoder) Write(buf *audio.IntBuffer) error {
	if err := e.writeSetup(); err != nil {
		return err
	}

	_, err := e.addBuffer(buf, nil)
	return err
}

func (e *Encoder) WriteAt(buf *audio.IntBuffer, pos int64) (int64, error) {
	if err := e.writeSetup(); err != nil {
		return 0, err
	}

	return e.addBuffer(buf, &pos)
}

func (e *Encoder) writeSetup() error {
	e.mu.Lock()
	if !e.wroteHeader {
		if err := e.writeHeader(); err != nil {
			e.mu.Unlock()
			return err
		}
	}

	if !e.pcmChunkStarted {
		// sound header
		if err := e.AddLE(riff.DataFormatID); err != nil {
			e.mu.Unlock()
			return fmt.Errorf("error encoding sound header %w", err)
		}
		e.pcmChunkStarted = true

		// write a temporary chunksize
		e.pcmChunkSizePos = e.WrittenBytes
		if err := e.AddLE(uint32(42)); err != nil {
			e.mu.Unlock()
			return fmt.Errorf("%w when writing wav data chunk size header", err)
		}

		e.pcmChunkPos = int64(e.WrittenBytes)
	}
	e.mu.Unlock()

	return nil
}

// WriteFrame writes a single frame of data to the underlying writer.
func (e *Encoder) WriteFrame(value interface{}) error {
	if !e.wroteHeader {
		e.writeHeader()
	}
	if !e.pcmChunkStarted {
		// sound header
		if err := e.AddLE(riff.DataFormatID); err != nil {
			return fmt.Errorf("error encoding sound header %w", err)
		}
		e.pcmChunkStarted = true

		// write a temporary chunksize
		e.pcmChunkSizePos = e.WrittenBytes
		if err := e.AddLE(uint32(42)); err != nil {
			return fmt.Errorf("%w when writing wav data chunk size header", err)
		}
	}

	e.frames++
	return e.AddLE(value)
}

func (e *Encoder) writeMetadata() error {
	chunkData := encodeInfoChunk(e)
	if err := e.AddBE(CIDList); err != nil {
		return fmt.Errorf("failed to write the LIST chunk ID: %w", err)
	}
	if err := e.AddLE(uint32(len(chunkData))); err != nil {
		return fmt.Errorf("failed to write the LIST chunk size: %w", err)
	}
	return e.AddBE(chunkData)
}

// Close flushes the content to disk, make sure the headers are up to date
// Note that the underlying writer is NOT being closed.
func (e *Encoder) Close() error {
	if e == nil || e.w == nil {
		return nil
	}

	// inject metadata at the end to not trip implementation not supporting
	// metadata chunks
	if e.Metadata != nil {
		if err := e.writeMetadata(); err != nil {
			return fmt.Errorf("failed to write metadata - %w", err)
		}
	}

	// go back and write total size in header
	if _, err := e.w.Seek(4, 0); err != nil {
		return err
	}
	if err := e.AddLE(uint32(e.WrittenBytes) - 8); err != nil {
		return fmt.Errorf("%w when writing the total written bytes", err)
	}

	// rewrite the audio chunk length header
	if e.pcmChunkSizePos > 0 {
		if _, err := e.w.Seek(int64(e.pcmChunkSizePos), 0); err != nil {
			return err
		}
		chunksize := uint32((int(e.BitDepth) / 8) * int(e.NumChans) * e.frames)
		if err := e.AddLE(uint32(chunksize)); err != nil {
			return fmt.Errorf("%w when writing wav data chunk size header", err)
		}
	}

	// jump back to the end of the file.
	if _, err := e.w.Seek(0, 2); err != nil {
		return err
	}
	switch e.w.(type) {
	case *os.File:
		return e.w.(*os.File).Sync()
	}
	return nil
}

package clientsim

import (
	"bytes"
	"context"
	"crypto/sha256"
	"encoding/binary"
	"errors"
	"io"
)

const (
	journalMagic  = "ICHAT-SENDLOG-1\n"
	frameMagic    = "SNP1"
	commitMagic   = "COMMIT01"
	frameHeader   = 16 + sha256.Size
	frameOverhead = frameHeader + sha256.Size + len(commitMagic)
	journalChunk  = 32 * 1024
)

func encodeJournalFrame(generation uint64, body []byte) []byte {
	frame := make([]byte, frameOverhead+len(body))
	copy(frame, frameMagic)
	binary.BigEndian.PutUint32(frame[4:8], uint32(len(body)))
	binary.BigEndian.PutUint64(frame[8:16], generation)
	headerHash := sha256.Sum256(frame[:16])
	copy(frame[16:frameHeader], headerHash[:])
	copy(frame[frameHeader:], body)
	end := frameHeader + len(body)
	hash := sha256.Sum256(frame[:end])
	copy(frame[end:], hash[:])
	copy(frame[end+sha256.Size:], commitMagic)
	return frame
}

func recoverJournal(ctx context.Context, file journalFile) ([]byte, int64, uint64, error) {
	info, err := file.Stat()
	if err != nil {
		return nil, 0, 0, safeError(ErrStore, err)
	}
	if !info.Mode().IsRegular() || info.Size() < int64(len(journalMagic)) || info.Size() > MaxJournalBytes {
		return nil, 0, 0, ErrSnapshot
	}
	magic := make([]byte, len(journalMagic))
	if err := readJournalAt(ctx, file, magic, 0); err != nil {
		return nil, 0, 0, safeError(ErrStore, err)
	}
	if string(magic) != journalMagic {
		return nil, 0, 0, ErrSnapshot
	}
	end := int64(len(journalMagic))
	var generation uint64
	var last []byte
	for end < info.Size() {
		if err := ctx.Err(); err != nil {
			return nil, 0, 0, err
		}
		remaining := info.Size() - end
		header := make([]byte, min(int64(frameHeader), remaining))
		if err := readJournalAt(ctx, file, header, end); err != nil {
			return nil, 0, 0, safeError(ErrStore, err)
		}
		if !validJournalHeaderPrefix(header, generation+1) {
			return nil, 0, 0, ErrSnapshot
		}
		if len(header) < frameHeader {
			break
		}
		length := int64(binary.BigEndian.Uint32(header[4:8]))
		if remaining < int64(frameOverhead)+length {
			break
		}
		frame := make([]byte, int64(frameOverhead)+length)
		copy(frame, header)
		if err := readJournalAt(ctx, file, frame[frameHeader:], end+frameHeader); err != nil {
			return nil, 0, 0, safeError(ErrStore, err)
		}
		payloadEnd := frameHeader + int(length)
		hash := sha256.Sum256(frame[:payloadEnd])
		if !bytes.Equal(hash[:], frame[payloadEnd:payloadEnd+sha256.Size]) || string(frame[payloadEnd+sha256.Size:]) != commitMagic {
			return nil, 0, 0, ErrSnapshot
		}
		last = append([]byte(nil), frame[frameHeader:payloadEnd]...)
		end += int64(len(frame))
		generation++
	}
	if end != info.Size() {
		if err := ctx.Err(); err != nil {
			return nil, 0, 0, err
		}
		if err := file.Truncate(end); err != nil {
			return nil, 0, 0, safeError(ErrStore, err)
		}
		if err := file.Sync(); err != nil {
			return nil, 0, 0, safeError(ErrStore, err)
		}
	}
	return last, end, generation, nil
}

// A complete header has its own checksum: a damaged length cannot disguise a
// committed frame as an incomplete tail. Available partial fields are checked.
func validJournalHeaderPrefix(header []byte, want uint64) bool {
	if !bytes.Equal(header[:min(len(header), len(frameMagic))], []byte(frameMagic)[:min(len(header), len(frameMagic))]) {
		return false
	}
	if len(header) >= 8 {
		length := binary.BigEndian.Uint32(header[4:8])
		if length == 0 || length > MaxSnapshotBytes {
			return false
		}
	}
	if len(header) > 8 {
		var expected [8]byte
		binary.BigEndian.PutUint64(expected[:], want)
		if !bytes.Equal(header[8:min(len(header), 16)], expected[:min(len(header)-8, 8)]) {
			return false
		}
	}
	if len(header) > 16 {
		hash := sha256.Sum256(header[:16])
		if !bytes.Equal(header[16:], hash[:len(header)-16]) {
			return false
		}
	}
	return true
}

func readJournalAt(ctx context.Context, file io.ReaderAt, body []byte, offset int64) error {
	for len(body) > 0 {
		if err := ctx.Err(); err != nil {
			return err
		}
		part := body[:min(len(body), journalChunk)]
		n, err := file.ReadAt(part, offset)
		if err != nil && !(errors.Is(err, io.EOF) && n == len(part)) {
			return err
		}
		if n != len(part) {
			return io.ErrUnexpectedEOF
		}
		body = body[n:]
		offset += int64(n)
	}
	return nil
}
func writeJournalAt(ctx context.Context, file io.WriterAt, body []byte, offset int64) error {
	for len(body) > 0 {
		if err := ctx.Err(); err != nil {
			return err
		}
		part := body[:min(len(body), journalChunk)]
		n, err := file.WriteAt(part, offset)
		if err != nil {
			return err
		}
		if n != len(part) {
			return io.ErrShortWrite
		}
		body = body[n:]
		offset += int64(n)
	}
	return nil
}

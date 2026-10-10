package messaging

import (
	"crypto/rand"
	"crypto/sha256"
	"encoding/binary"
	"encoding/hex"
	"regexp"
	"strconv"
	"strings"
	"time"
	"unicode/utf8"
)

// time.Parse accepts some non-RFC spellings and truncates excess fractional
// digits. Keep the wire timestamp grammar explicit and lossless.
var timestampPattern = regexp.MustCompile(`^[0-9]{4}-[0-9]{2}-[0-9]{2}T[0-9]{2}:[0-9]{2}:[0-9]{2}(\.[0-9]{1,9})?(Z|[+-]([01][0-9]|2[0-3]):[0-5][0-9])$`)

// digest uses a versioned domain followed by uint32 big-endian UTF-8 byte
// lengths and field bytes. Field order and domains are protocol constants.
func digest(domain string, fields ...string) string {
	h := sha256.New()
	h.Write([]byte(domain))
	var size [4]byte
	for _, field := range fields {
		binary.BigEndian.PutUint32(size[:], uint32(len(field)))
		h.Write(size[:])
		h.Write([]byte(field))
	}
	return hex.EncodeToString(h.Sum(nil))
}

func decimal(value string, allowZero bool) (uint64, error) {
	if len(value) == 0 || len(value) > 20 {
		return 0, invalid("decimal ID")
	}
	for i := range value {
		if value[i] < '0' || value[i] > '9' {
			return 0, invalid("decimal ID")
		}
	}
	n, err := strconv.ParseUint(value, 10, 64)
	if err != nil || (!allowZero && n == 0) {
		return 0, invalid("decimal ID")
	}
	return n, nil
}

// UUIDs must use the 36-character hyphenated spelling; case is normalized.
// Nil UUIDs are invalid message identities. No UUID version is required from
// clients; new physical publish attempts use random version 4 UUIDs.
func normalizeUUID(value string) (string, error) {
	if len(value) != 36 {
		return "", invalid("UUID")
	}
	nonzero := false
	for i := 0; i < len(value); i++ {
		c := value[i]
		if i == 8 || i == 13 || i == 18 || i == 23 {
			if c != '-' {
				return "", invalid("UUID")
			}
			continue
		}
		if !((c >= '0' && c <= '9') || (c >= 'a' && c <= 'f') || (c >= 'A' && c <= 'F')) {
			return "", invalid("UUID")
		}
		nonzero = nonzero || c != '0'
	}
	if !nonzero {
		return "", invalid("UUID")
	}
	return strings.ToLower(value), nil
}

func timestamp(value time.Time) (string, error) {
	value = value.UTC()
	if value.IsZero() || value.Year() < 1 || value.Year() > 9999 {
		return "", invalid("timestamp")
	}
	return value.Format(time.RFC3339Nano), nil
}

func normalizeTime(value string) (string, error) {
	if value == "" {
		return "", nil
	}
	if !timestampPattern.MatchString(value) {
		return "", invalid("timestamp")
	}
	t, err := time.Parse(time.RFC3339Nano, value)
	if err != nil {
		return "", invalid("timestamp")
	}
	return timestamp(t)
}

// Normalize is shared by the API and consumer. senderID must come from the
// authenticated context at ingress, never from decoded client JSON.
func Normalize(senderID uint64, request SendRequest) (Command, error) {
	if senderID == 0 {
		return Command{}, invalid("senderId")
	}
	id, err := normalizeUUID(request.ClientMessageID)
	if err != nil {
		return Command{}, invalid("clientMessageId")
	}
	target, err := decimal(request.TargetID, false)
	if err != nil {
		return Command{}, invalid("targetId")
	}
	if request.TargetType != Private && request.TargetType != Group {
		return Command{}, invalid("targetType")
	}
	if request.Media != Text && request.Media != Picture && request.Media != Audio {
		return Command{}, invalid("media")
	}
	if !utf8.ValidString(request.Content) || len(request.Content) == 0 || utf8.RuneCountInString(request.Content) > MaxContentRunes {
		return Command{}, invalid("content")
	}
	sentAt, err := normalizeTime(request.ClientSentAt)
	if err != nil {
		return Command{}, invalid("clientSentAt")
	}
	senderText, targetText := strconv.FormatUint(senderID, 10), strconv.FormatUint(target, 10)
	conversation := "g:" + targetText
	if request.TargetType == Private {
		low, high := senderText, targetText
		if senderID > target {
			low, high = high, low
		}
		conversation = "p:" + low + ":" + high
	}
	return Command{
		SchemaVersion: SchemaVersion, ClientMessageID: id,
		CommandID:   digest("ichat:command:v1\x00", senderText, id),
		PayloadHash: digest("ichat:payload:v1\x00", strconv.Itoa(SchemaVersion), senderText, string(request.TargetType), targetText, string(request.Media), request.Content),
		SenderID:    senderText, TargetType: request.TargetType, TargetID: targetText,
		Media: request.Media, Content: request.Content, ConversationID: conversation, ClientSentAt: sentAt,
	}, nil
}

func ValidateCommand(command Command) error {
	if command.SchemaVersion != SchemaVersion {
		return ErrUnsupportedVersion
	}
	sender, err := decimal(command.SenderID, false)
	if err != nil {
		return invalid("senderId")
	}
	expected, err := Normalize(sender, SendRequest{
		ClientMessageID: command.ClientMessageID, TargetType: command.TargetType,
		TargetID: command.TargetID, Media: command.Media, Content: command.Content, ClientSentAt: command.ClientSentAt,
	})
	if err != nil {
		return err
	}
	if command != expected {
		return ErrIdentityMismatch
	}
	return nil
}

func newAttemptID() (string, error) {
	var data [16]byte
	if _, err := rand.Read(data[:]); err != nil {
		return "", err
	}
	data[6] = (data[6] & 0x0f) | 0x40
	data[8] = (data[8] & 0x3f) | 0x80
	s := hex.EncodeToString(data[:])
	return s[:8] + "-" + s[8:12] + "-" + s[12:16] + "-" + s[16:20] + "-" + s[20:], nil
}

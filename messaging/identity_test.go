package messaging

import (
	"encoding/json"
	"errors"
	"math"
	"strings"
	"testing"
	"time"
)

func exampleRequest() SendRequest {
	return SendRequest{ClientMessageID: "8fd7c2ef-ae26-4bb5-91a5-32a302bb05f1", TargetType: Private, TargetID: "34", Media: Text, Content: "hello"}
}

func exampleCommand(t *testing.T) Command {
	t.Helper()
	c, err := Normalize(12, exampleRequest())
	if err != nil {
		t.Fatal(err)
	}
	return c
}

func TestIdentityGoldenAndNormalization(t *testing.T) {
	c := exampleCommand(t)
	// Independently computed with Python hashlib + struct.pack('>I', byteLen).
	if c.CommandID != "55faac68e13e0a6d5b9562536d3b0c7cf90552416747c9986500b2202fe96fa2" ||
		c.PayloadHash != "55405570ca8bd409cbc5ed2f2b5914c09b4b5141fb4b377ef6d46045550ad591" {
		t.Fatalf("protocol hash changed: %+v", c)
	}
	if c.ConversationID != "p:12:34" || c.SenderID != "12" || c.SchemaVersion != 1 {
		t.Fatalf("command=%+v", c)
	}
	unicodeRequest := exampleRequest()
	unicodeRequest.Content = " \r\n中😀\x00"
	unicodeCommand, err := Normalize(12, unicodeRequest)
	if err != nil || unicodeCommand.PayloadHash != "db3a3b345db4485e071be38f136425b0d3489769106319e83dd662242e88f273" {
		t.Fatalf("UTF-8 byte lengths or content changed: %+v %v", unicodeCommand, err)
	}
	r := exampleRequest()
	r.ClientMessageID = strings.ToUpper(r.ClientMessageID)
	r.TargetID = "00034"
	canonical, err := Normalize(12, r)
	if err != nil || canonical != c {
		t.Fatalf("normalization=%+v err=%v", canonical, err)
	}
	r.ClientSentAt = "2026-10-10T12:34:56.123456789+11:00"
	metadata, err := Normalize(12, r)
	if err != nil || metadata.ClientSentAt != "2026-10-10T01:34:56.123456789Z" || metadata.CommandID != c.CommandID || metadata.PayloadHash != c.PayloadHash {
		t.Fatalf("client metadata changed business identity: %+v %v", metadata, err)
	}
	r.ClientMessageID = "11111111-1111-4111-8111-111111111111"
	differentKey, err := Normalize(12, r)
	if err != nil || differentKey.CommandID == c.CommandID || differentKey.PayloadHash != c.PayloadHash {
		t.Fatalf("different logical send=%+v %v", differentKey, err)
	}
	otherSender, err := Normalize(13, exampleRequest())
	if err != nil || otherSender.CommandID == c.CommandID || otherSender.PayloadHash == c.PayloadHash {
		t.Fatal("sender missing from identity/hash")
	}
	if digest("domain\x00", "a", "bc") == digest("domain\x00", "ab", "c") ||
		digest("domain\x00", "a\x00", "b") == digest("domain\x00", "a", "\x00b") ||
		digest("domain\x00", "x") == digest("other\x00", "x") {
		t.Fatal("ambiguous encoding")
	}
}

func TestImmutablePayloadFields(t *testing.T) {
	base := exampleCommand(t)
	for name, change := range map[string]func(*SendRequest){
		"target":   func(r *SendRequest) { r.TargetID = "35" },
		"type":     func(r *SendRequest) { r.TargetType = Group },
		"picture":  func(r *SendRequest) { r.Media = Picture },
		"audio":    func(r *SendRequest) { r.Media = Audio },
		"content":  func(r *SendRequest) { r.Content = "hello!" },
		"no_trim":  func(r *SendRequest) { r.Content = " hello " },
		"newlines": func(r *SendRequest) { r.Content = "hello\r\n" },
	} {
		t.Run(name, func(t *testing.T) {
			r := exampleRequest()
			change(&r)
			got, err := Normalize(12, r)
			if err != nil || got.CommandID != base.CommandID || got.PayloadHash == base.PayloadHash || got.Content != r.Content {
				t.Fatalf("mutation=%+v %v", got, err)
			}
		})
	}
	r := exampleRequest()
	r.Content = "é"
	nfc, _ := Normalize(12, r)
	r.Content = "e\u0301"
	nfd, _ := Normalize(12, r)
	if nfc.PayloadHash == nfd.PayloadHash || nfd.Content != r.Content {
		t.Fatal("Unicode was normalized")
	}
}

func TestConversationIDsAndLargeIDs(t *testing.T) {
	for _, tc := range []struct {
		sender uint64
		target string
		kind   TargetType
		want   string
	}{
		{12, "2", Private, "p:2:12"}, {2, "12", Private, "p:2:12"},
		{12, "12", Private, "p:12:12"}, // Business permission is decided later.
		{12, "2", Group, "g:2"}, {2, "12", Group, "g:12"},
		{math.MaxUint64, "9007199254740993", Private, "p:9007199254740993:18446744073709551615"},
	} {
		r := exampleRequest()
		r.TargetID, r.TargetType = tc.target, tc.kind
		got, err := Normalize(tc.sender, r)
		if err != nil || got.ConversationID != tc.want {
			t.Fatalf("conversation=%s want=%s err=%v", got.ConversationID, tc.want, err)
		}
	}
}

func TestInputLimits(t *testing.T) {
	for name, change := range map[string]func(*SendRequest){
		"empty_uuid":       func(r *SendRequest) { r.ClientMessageID = "" },
		"nil_uuid":         func(r *SendRequest) { r.ClientMessageID = "00000000-0000-0000-0000-000000000000" },
		"uuid_braces":      func(r *SendRequest) { r.ClientMessageID = "{" + r.ClientMessageID + "}" },
		"uuid_compact":     func(r *SendRequest) { r.ClientMessageID = strings.ReplaceAll(r.ClientMessageID, "-", "") },
		"uuid_hex":         func(r *SendRequest) { r.ClientMessageID = "z" + r.ClientMessageID[1:] },
		"uuid_separator":   func(r *SendRequest) { r.ClientMessageID = r.ClientMessageID[:8] + "_" + r.ClientMessageID[9:] },
		"target_zero":      func(r *SendRequest) { r.TargetID = "0" },
		"target_empty":     func(r *SendRequest) { r.TargetID = "" },
		"target_negative":  func(r *SendRequest) { r.TargetID = "-1" },
		"target_sign":      func(r *SendRequest) { r.TargetID = "+1" },
		"target_space":     func(r *SendRequest) { r.TargetID = " 1" },
		"target_decimal":   func(r *SendRequest) { r.TargetID = "1.0" },
		"target_exponent":  func(r *SendRequest) { r.TargetID = "1e2" },
		"target_unicode":   func(r *SendRequest) { r.TargetID = "１" },
		"target_overflow":  func(r *SendRequest) { r.TargetID = "18446744073709551616" },
		"target_unbounded": func(r *SendRequest) { r.TargetID = strings.Repeat("0", 20) + "1" },
		"target_type":      func(r *SendRequest) { r.TargetType = "Private" },
		"legacy_type":      func(r *SendRequest) { r.TargetType = "2" },
		"media":            func(r *SendRequest) { r.Media = "video" },
		"legacy_media":     func(r *SendRequest) { r.Media = "0" },
		"empty_content":    func(r *SendRequest) { r.Content = "" },
		"long_ascii":       func(r *SendRequest) { r.Content = strings.Repeat("a", 1025) },
		"long_utf8":        func(r *SendRequest) { r.Content = strings.Repeat("😀", 1025) },
		"invalid_utf8":     func(r *SendRequest) { r.Content = "a\xff" },
		"timestamp":        func(r *SendRequest) { r.ClientSentAt = "not a time" },
	} {
		t.Run(name, func(t *testing.T) {
			r := exampleRequest()
			change(&r)
			got, err := Normalize(12, r)
			if !errors.Is(err, ErrInvalidInput) || got != (Command{}) {
				t.Fatalf("accepted invalid input: %+v %v", got, err)
			}
		})
	}
	if _, err := Normalize(0, exampleRequest()); !errors.Is(err, ErrInvalidInput) {
		t.Fatal("zero sender accepted")
	}
	for _, content := range []string{strings.Repeat("a", 1024), strings.Repeat("中", 1024), strings.Repeat("😀", 1024), " \r\n\t", "a\x00b", "�"} {
		r := exampleRequest()
		r.Content = content
		got, err := Normalize(12, r)
		if err != nil || got.Content != content {
			t.Fatalf("valid UTF-8 was changed: %v", err)
		}
	}
}

func TestSequenceJSONAndMessageDTO(t *testing.T) {
	for _, n := range []uint64{0, 1, 9007199254740993, math.MaxUint64} {
		seq := Sequence(n)
		body, err := json.Marshal(seq)
		if err != nil || string(body) != `"`+seq.String()+`"` {
			t.Fatalf("sequence JSON=%s %v", body, err)
		}
		var decoded Sequence
		if err := json.Unmarshal(body, &decoded); err != nil || decoded != seq {
			t.Fatalf("sequence lost precision: %s %v", body, err)
		}
	}
	for _, raw := range []string{`null`, `42`, `""`, `"01"`, `"-1"`, `"+1"`, `"1.0"`, `"1e3"`, `" 1"`, `"18446744073709551616"`} {
		seq := Sequence(7)
		if err := json.Unmarshal([]byte(raw), &seq); !errors.Is(err, ErrInvalidInput) || seq != 7 {
			t.Fatalf("invalid seq %s: %d %v", raw, seq, err)
		}
	}
	c := exampleCommand(t)
	stamp := time.Date(2026, 10, 10, 12, 0, 0, 123456789, time.FixedZone("offset", 11*3600))
	message, err := NewMessageDTO(c, Sequence(math.MaxUint64), stamp)
	if err != nil || message.MessageID != c.CommandID || message.Seq != Sequence(math.MaxUint64) || message.ServerCreatedAt != "2026-10-10T01:00:00.123456789Z" || message.Content != c.Content {
		t.Fatalf("message=%+v %v", message, err)
	}
	body, _ := json.Marshal(message)
	if !strings.Contains(string(body), `"seq":"18446744073709551615"`) || !strings.Contains(string(body), `"senderId":"12"`) {
		t.Fatalf("unsafe JSON integers: %s", body)
	}
	if _, err := NewMessageDTO(c, 0, stamp); !errors.Is(err, ErrInvalidInput) {
		t.Fatal("unallocated sequence accepted")
	}
	if _, err := NewMessageDTO(c, 1, time.Time{}); !errors.Is(err, ErrInvalidInput) {
		t.Fatal("zero timestamp accepted")
	}
	c.ConversationID = "g:999"
	if _, err := NewMessageDTO(c, 1, stamp); !errors.Is(err, ErrIdentityMismatch) {
		t.Fatal("tampered conversation accepted")
	}
}

func TestTimestampGrammar(t *testing.T) {
	for _, value := range []string{
		"2026-10-10T1:02:03Z", "2026-10-10T01:02:03,5Z",
		"2026-10-10T01:02:03.1234567891Z", "2026-10-10T01:02:03+24:00",
		"2026-10-10T01:02:03+01:60", "2026-02-30T01:02:03Z",
		"2026-10-10t01:02:03z", "0001-01-01T00:00:00Z",
	} {
		r := exampleRequest()
		r.ClientSentAt = value
		if _, err := Normalize(12, r); !errors.Is(err, ErrInvalidInput) {
			t.Fatalf("invalid timestamp %q accepted: %v", value, err)
		}
	}
	for _, value := range []string{"2026-10-10T01:02:03.1200Z", "2026-10-10T12:02:03.12+11:00"} {
		r := exampleRequest()
		r.ClientSentAt = value
		c, err := Normalize(12, r)
		if err != nil || c.ClientSentAt != "2026-10-10T01:02:03.12Z" {
			t.Fatalf("timestamp normalization=%q %v", c.ClientSentAt, err)
		}
	}
}

package messaging

import (
	"encoding/json"
	"errors"
	"strings"
	"testing"
	"time"
)

func TestEnvelopeRetriesAndTampering(t *testing.T) {
	c := exampleCommand(t)
	stamp := time.Date(2026, 10, 10, 1, 2, 3, 0, time.UTC)
	seen := map[string]bool{}
	var envelope Envelope
	for i := 0; i < 64; i++ {
		var err error
		envelope, err = NewEnvelope(c, stamp.Add(time.Duration(i)*time.Second))
		if err != nil {
			t.Fatal(err)
		}
		id := envelope.PublishAttemptID
		if seen[id] || id[14] != '4' || !strings.ContainsRune("89ab", rune(id[19])) {
			t.Fatalf("invalid/reused attempt: %s", id)
		}
		seen[id] = true
		body, err := json.Marshal(envelope)
		if err != nil {
			t.Fatal(err)
		}
		decoded, err := DecodeEnvelope(body)
		if err != nil || decoded != envelope {
			t.Fatalf("envelope roundtrip=%+v %v", decoded, err)
		}
		got, err := ValidateEnvelope(decoded)
		if err != nil || got != c {
			t.Fatalf("retry changed command=%+v %v", got, err)
		}
	}
	for name, change := range map[string]func(*Envelope){
		"command_id":          func(e *Envelope) { e.CommandID = strings.Repeat("a", 64) },
		"hash":                func(e *Envelope) { e.PayloadHash = strings.Repeat("a", 64) },
		"hash_case":           func(e *Envelope) { e.PayloadHash = strings.ToUpper(e.PayloadHash) },
		"sender":              func(e *Envelope) { e.SenderID = "13" },
		"target":              func(e *Envelope) { e.TargetID = "35" },
		"target_type":         func(e *Envelope) { e.TargetType = Group },
		"media":               func(e *Envelope) { e.Media = Audio },
		"content":             func(e *Envelope) { e.Content += "!" },
		"client_id":           func(e *Envelope) { e.ClientMessageID = "11111111-1111-4111-8111-111111111111" },
		"uuid_case":           func(e *Envelope) { e.ClientMessageID = strings.ToUpper(e.ClientMessageID) },
		"target_noncanonical": func(e *Envelope) { e.TargetID = "034" },
		"sender_noncanonical": func(e *Envelope) { e.SenderID = "012" },
		"attempt":             func(e *Envelope) { e.PublishAttemptID = "" },
		"time":                func(e *Envelope) { e.ReceivedAt = "" },
		"time_offset":         func(e *Envelope) { e.ReceivedAt = "2026-10-10T12:02:03+11:00" },
		"client_time":         func(e *Envelope) { e.ClientSentAt = "2026-10-10T12:02:03+11:00" },
		"schema":              func(e *Envelope) { e.SchemaVersion++ },
	} {
		t.Run(name, func(t *testing.T) {
			bad := envelope
			change(&bad)
			if command, err := ValidateEnvelope(bad); err == nil || command != (Command{}) {
				t.Fatalf("accepted tampering: %+v %v", command, err)
			}
			body, _ := json.Marshal(bad)
			if _, err := DecodeEnvelope(body); err == nil {
				t.Fatal("wire decoder accepted tampering")
			}
		})
	}
	envelope.ClientSentAt = "2026-10-10T01:02:03Z"
	if got, err := ValidateEnvelope(envelope); err != nil || got.PayloadHash != c.PayloadHash || got.ClientSentAt != envelope.ClientSentAt {
		t.Fatalf("metadata retry rejected: %+v %v", got, err)
	}
	for _, stamp := range []time.Time{{}, time.Date(10000, 1, 1, 0, 0, 0, 0, time.UTC)} {
		if _, err := NewEnvelope(c, stamp); !errors.Is(err, ErrInvalidInput) {
			t.Fatal("invalid server timestamp accepted")
		}
	}
	c.SchemaVersion++
	if _, err := NewEnvelope(c, time.Now()); !errors.Is(err, ErrUnsupportedVersion) {
		t.Fatal("unsupported schema accepted")
	}
}

func TestStrictRequestJSON(t *testing.T) {
	for name, body := range map[string]string{
		"unknown_sender": `{"senderId":"12"}`, "unknown_legacy": `{"fromUser":{"id":12}}`,
		"case_alias": `{"Content":"hello"}`, "duplicate": `{"content":"one","content":"two"}`,
		"escaped_duplicate": `{"content":"one","cont\u0065nt":"two"}`,
		"null":              `null`, "array": `[]`, "two_objects": `{} {}`, "trailing": `{}garbage`,
		"null_content": `{"content":null}`, "null_optional": `{"clientSentAt":null}`,
		"numeric_id": `{"targetId":34}`, "numeric_enum": `{"media":0}`,
		"invalid_utf8":   "{\"content\":\"\xff\"}",
		"high_surrogate": `{"content":"\ud800"}`, "low_surrogate": `{"content":"\udfff"}`,
		"wrong_pair": `{"content":"\ud800\u0041"}`, "trailing_pair": `{"content":"\ud800x"}`,
		"bad_key": `{"\ud800":"hello"}`, "invalid_escape": `{"content":"\uxxxx"}`,
		"oversized": strings.Repeat(" ", MaxBodyBytes) + `{}`,
	} {
		t.Run(name, func(t *testing.T) {
			if got, err := DecodeRequest([]byte(body)); !errors.Is(err, ErrInvalidInput) || got != (SendRequest{}) {
				t.Fatalf("invalid JSON accepted: %+v %v", got, err)
			}
		})
	}
	for _, tc := range []struct{ raw, want string }{
		{`"中😀"`, "中😀"}, {`"\u4e2d\ud83d\ude00"`, "中😀"},
		{`"\ufffd"`, "�"}, {`"a\u0000b"`, "a\x00b"},
		{`"\\ud800"`, `\ud800`}, {`"\"\\/\r\n"`, "\"\\/\r\n"},
	} {
		body := `{"clientMessageId":"8fd7c2ef-ae26-4bb5-91a5-32a302bb05f1","targetType":"private","targetId":"34","media":"text","content":` + tc.raw + `}`
		request, err := DecodeRequest([]byte(body))
		if err != nil || request.Content != tc.want {
			t.Fatalf("content changed: %q want %q err=%v", request.Content, tc.want, err)
		}
		got, err := Normalize(12, request)
		if err != nil || got.Content != tc.want {
			t.Fatalf("content changed on normalize: %q %v", got.Content, err)
		}
	}
	// The body bound counts bytes, independently of Unicode character limits.
	base, _ := json.Marshal(exampleRequest())
	padded := append(base, []byte(strings.Repeat(" ", MaxBodyBytes-len(base)))...)
	if _, err := DecodeRequest(padded); err != nil {
		t.Fatalf("exact body limit rejected: %v", err)
	}
	if _, err := DecodeRequest(append(padded, ' ')); !errors.Is(err, ErrInvalidInput) {
		t.Fatal("body limit not enforced")
	}
	// Escaped and literal JSON spellings produce the same decoded business text.
	literal := exampleRequest()
	literal.Content = "中😀"
	want, _ := Normalize(12, literal)
	escaped, _ := DecodeRequest([]byte(`{"clientMessageId":"8fd7c2ef-ae26-4bb5-91a5-32a302bb05f1","targetType":"private","targetId":"34","media":"text","content":"\u4e2d\ud83d\ude00"}`))
	got, err := Normalize(12, escaped)
	if err != nil || got != want {
		t.Fatal("JSON escape spelling changed hash")
	}
}

func FuzzRequestNormalization(f *testing.F) {
	seed, _ := json.Marshal(exampleRequest())
	f.Add(seed)
	f.Add([]byte(`{"content":"\ud800"}`))
	f.Add([]byte(`{"content":"\\\"\ud83d\ude00"}`))
	f.Fuzz(func(t *testing.T, body []byte) {
		r, err := DecodeRequest(body)
		if err != nil {
			return
		}
		c, err := Normalize(12, r)
		if err != nil {
			return
		}
		if err := ValidateCommand(c); err != nil {
			t.Fatal(err)
		}
		reencoded, err := json.Marshal(r)
		if err != nil {
			t.Fatal(err)
		}
		roundtrip, err := DecodeRequest(reencoded)
		if err != nil {
			t.Fatal(err)
		}
		got, err := Normalize(12, roundtrip)
		if err != nil || got != c {
			t.Fatal("roundtrip changed canonical command")
		}
	})
}

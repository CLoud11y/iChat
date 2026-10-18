package main

import (
	"bytes"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net/http"
	"net/url"
	"os"
	"strings"
	"time"

	"iChat/messaging"
)

var errAcceptanceProbeOptions = errors.New("invalid acceptance probe options or request")

type acceptanceProbeReport struct {
	Mode            string `json:"mode"`
	Attempts        int    `json:"attempts"`
	Accepted        int    `json:"accepted"`
	Unknown         int    `json:"publish_unknown"`
	NotPublished    int    `json:"not_published"`
	Rejected        int    `json:"not_accepted"`
	State           string `json:"state"`
	ClientMessageID string `json:"clientMessageId"`
	CommandID       string `json:"commandId"`
	PayloadHash     string `json:"payloadHash"`
}

// This is an acceptance-only probe, not persistence/delivery throughput. The
// supplied request is already on disk before network I/O. Unknown responses,
// lost HTTP responses and known accepted never delete or rewrite that request.
func acceptanceProbe(opt options, token string) (acceptanceProbeReport, error) {
	var report acceptanceProbeReport
	base, err := url.Parse(opt.baseURL)
	if err != nil || base.Host == "" || (base.Scheme != "http" && base.Scheme != "https") || base.User != nil || base.RawQuery != "" || base.Fragment != "" || opt.requestFile == "" || opt.senderID == 0 || opt.attempts < 1 || opt.attempts > 100 || opt.requestTimeout <= 0 || opt.requestTimeout > time.Minute || token == "" || len(token) > 8192 || strings.ContainsAny(token, " \r\n\t") {
		return report, errAcceptanceProbeOptions
	}
	file, err := os.Open(opt.requestFile)
	if err != nil {
		return report, errAcceptanceProbeOptions
	}
	if opt.output != "" {
		source, sourceErr := file.Stat()
		target, targetErr := os.Stat(opt.output)
		if sourceErr != nil || targetErr == nil && os.SameFile(source, target) {
			file.Close()
			return report, errAcceptanceProbeOptions
		}
	}
	body, readErr := io.ReadAll(io.LimitReader(file, messaging.MaxBodyBytes+1))
	file.Close()
	if readErr != nil || len(body) > messaging.MaxBodyBytes {
		return report, errAcceptanceProbeOptions
	}
	request, err := messaging.DecodeRequest(body)
	if err != nil {
		return report, errAcceptanceProbeOptions
	}
	command, err := messaging.Normalize(opt.senderID, request)
	if err != nil {
		return report, errAcceptanceProbeOptions
	}
	report = acceptanceProbeReport{Mode: "acceptance", ClientMessageID: command.ClientMessageID, CommandID: command.CommandID, PayloadHash: command.PayloadHash}
	transport := &http.Transport{MaxConnsPerHost: 1, MaxIdleConnsPerHost: 1}
	defer transport.CloseIdleConnections()
	client := &http.Client{Timeout: opt.requestTimeout, Transport: transport, CheckRedirect: func(*http.Request, []*http.Request) error { return http.ErrUseLastResponse }}
	for range opt.attempts {
		report.Attempts++
		req, err := http.NewRequest(http.MethodPost, strings.TrimRight(opt.baseURL, "/")+"/auth/sendMessage", bytes.NewReader(body))
		if err != nil {
			return report, errAcceptanceProbeOptions
		}
		req.Header.Set("Authorization", "Bearer "+token)
		req.Header.Set("Content-Type", "application/json")
		response, err := client.Do(req)
		if err != nil {
			report.Unknown++
			continue
		}
		wire, readErr := io.ReadAll(io.LimitReader(response.Body, messaging.MaxBodyBytes+1))
		response.Body.Close()
		var decoded messaging.SubmissionResponse
		if readErr != nil || len(wire) > messaging.MaxBodyBytes || json.Unmarshal(wire, &decoded) != nil {
			report.Unknown++
			continue
		}
		switch classifySubmissionResponse(response.StatusCode, decoded, command) {
		case "accepted":
			report.Accepted++
		case "not_published":
			report.NotPublished++
		case "not_accepted":
			report.Rejected++
		default:
			report.Unknown++
		}
	}
	switch {
	case report.Accepted != 0:
		report.State = "accepted"
	case report.Unknown != 0:
		report.State = "publish_unknown"
	case report.NotPublished == report.Attempts:
		report.State = "not_published"
	default:
		report.State = "not_accepted"
	}
	return report, nil
}

func classifySubmissionResponse(status int, response messaging.SubmissionResponse, command messaging.Command) string {
	d := response.Data
	if status == 202 && response.Code == 0 && response.Msg == "accepted" && d.State == "accepted" && d.ReasonCode == "" && d.Matches(command) && messaging.ValidPublishAttemptID(d.PublishAttemptID) {
		return "accepted"
	}
	if response.Code != -1 {
		return "publish_unknown"
	}
	if (status == 429 && d.ReasonCode == "OVERLOADED" || status == 503 && d.ReasonCode == "DEPENDENCY_UNAVAILABLE") && d.State == "not_published" && d.Matches(command) {
		return "not_published"
	}
	if status == 503 && d.Matches(command) && messaging.ValidPublishAttemptID(d.PublishAttemptID) && (d.State == "unroutable" && d.ReasonCode == "PUBLISH_UNROUTABLE" || d.State == "nacked" && d.ReasonCode == "DEPENDENCY_UNAVAILABLE") {
		return "not_accepted"
	}
	if (status == 400 && d.ReasonCode == "INVALID_REQUEST" || status == 401 && d.ReasonCode == "UNAUTHENTICATED") && (d.State == "" || d.State == "not_published") {
		return "not_accepted"
	}
	return "publish_unknown"
}

func runAcceptanceProbe(opt options, token string) error {
	report, err := acceptanceProbe(opt, token)
	if err != nil {
		return err
	}
	wire, err := json.MarshalIndent(report, "", "  ")
	if err != nil {
		return err
	}
	fmt.Println(string(wire))
	if opt.output != "" {
		return os.WriteFile(opt.output, append(wire, '\n'), 0600)
	}
	return nil
}

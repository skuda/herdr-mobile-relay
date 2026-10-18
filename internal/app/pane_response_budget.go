package app

import (
	"encoding/json"
	"strings"

	"github.com/0cv/herdr-mobile-relay/internal/noecho"
	"github.com/0cv/herdr-mobile-relay/internal/panedelta"
	"github.com/0cv/herdr-mobile-relay/internal/transport"
)

const PaneResponseBudgetBytes = transport.MaxOutboundMessageBytes - 64*1024

// paneResponseFits reserves the fingerprint and watch acknowledgement before encoding.
func paneResponseFits(response map[string]any) bool {
	candidate := make(map[string]any, len(response)+3)
	for key, value := range response {
		candidate[key] = value
	}
	candidate["content_fingerprint"] = "0123456789abcdef"
	candidate["ack_required"] = true
	encoded, err := json.Marshal(candidate)
	return err == nil && len(encoded) <= PaneResponseBudgetBytes
}

func fittedPaneDelta(response map[string]any, base string, segments []panedelta.Segment) map[string]any {
	delta := paneDeltaResponse(response, base, segments)
	if paneResponseFits(delta) {
		return delta
	}
	response = fitPaneContentResponse(response)
	response["ack_required"] = true
	return response
}

func fitPaneContentResponse(response map[string]any) map[string]any {
	content, ok := successfulPaneContent(response)
	if !ok {
		return response
	}
	setContent := func(content string) {
		response["content"] = content
		delete(response, "no_echo_prompt")
		prompt, secret := noecho.Match(content)
		response["no_echo"] = secret
		if secret {
			response["no_echo_prompt"] = prompt
		}
		if content == "" {
			for _, key := range []string{"attention_kind", "prompt", "command", "options", "interaction", "question_layout"} {
				delete(response, key)
			}
		}
	}
	setContent(content)
	if paneResponseFits(response) {
		return response
	}
	response["truncated"] = true
	lines := strings.Split(content, "\n")
	low, high := 0, len(lines)
	for low < high {
		mid := low + (high-low+1)/2
		setContent(strings.Join(lines[len(lines)-mid:], "\n"))
		if paneResponseFits(response) {
			low = mid
		} else {
			high = mid - 1
		}
	}
	setContent(strings.Join(lines[len(lines)-low:], "\n"))
	return response
}

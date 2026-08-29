package orderevents

import (
	"encoding/json"
	"testing"
)

func TestDecodeSQSBodySupportsRawAndSNSWrappedDelivery(t *testing.T) {
	event := validPlacedEvent()
	payload, _ := json.Marshal(event)

	raw, decoded, err := decodeSQSBody(string(payload))
	if err != nil || decoded.EventID != event.EventID || string(raw) != string(payload) {
		t.Fatalf("raw decode: event=%#v err=%v", decoded, err)
	}
	wrapper, _ := json.Marshal(map[string]any{
		"Type": "Notification", "TopicArn": "arn:example", "Message": string(payload),
	})
	raw, decoded, err = decodeSQSBody(string(wrapper))
	if err != nil || decoded.EventID != event.EventID || string(raw) != string(payload) {
		t.Fatalf("wrapped decode: event=%#v err=%v", decoded, err)
	}
}

func TestDecodeSQSBodyRejectsUnknownEventFields(t *testing.T) {
	event := validPlacedEvent()
	payload, _ := json.Marshal(event)
	var object map[string]any
	_ = json.Unmarshal(payload, &object)
	object["unexpected_trust_boundary_field"] = true
	mutated, _ := json.Marshal(object)
	if _, _, err := decodeSQSBody(string(mutated)); err == nil {
		t.Fatal("unknown event field was accepted")
	}
}

package sqsconsumer

import (
	"context"
	"slices"
	"strings"
	"testing"
	"time"

	"github.com/aws/aws-sdk-go-v2/aws"
	"github.com/aws/aws-sdk-go-v2/service/sqs/types"
)

func TestRedriveSkipsPermanentAndKeepsGroupAndBody(t *testing.T) {
	permanent := msg("p", "w1", 10)
	permanent.MessageAttributes = map[string]types.MessageAttributeValue{AttrFailureClass: strAttr(FailureClassPermanent)}
	exhausted1, exhausted2 := msg("e1", "w1", 10), msg("e2", "w2", 10)
	api := newFakeSQS([]types.Message{permanent, exhausted1}, []types.Message{exhausted2})
	api.nonBlocking = true

	now := func() time.Time { return time.Unix(1_800_000_000, 0) }
	res, err := Redrive(context.Background(), api, "url/dlq.fifo", "url/in.fifo", operator, now)
	if err != nil {
		t.Fatal(err)
	}
	if res.Redriven != 2 || res.Skipped != 1 {
		t.Fatalf("result %+v", res)
	}
	deleted, _, sent := api.snapshot()
	if strings.Join(deleted, ",") != "e1,e2" {
		t.Fatalf("deleted %v (the permanent message must stay)", deleted)
	}
	for i, want := range []types.Message{exhausted1, exhausted2} {
		in := sent[i]
		if aws.ToString(in.QueueUrl) != "url/in.fifo" || aws.ToString(in.MessageBody) != aws.ToString(want.Body) ||
			aws.ToString(in.MessageGroupId) != want.Attributes["MessageGroupId"] ||
			aws.ToString(in.MessageDeduplicationId) != aws.ToString(want.MessageId)+":redrive:1800000000" {
			t.Errorf("send %d: %+v", i, in)
		}
	}
}

// TestRedriveNamesTheOriginalSender: the resend's SenderId is the
// operator's, so it carries the original producer in originalSenderId
// (ARCHITECTURE §10). Only a message the operator already resent keeps the
// attribute it carries; any other sender's attribute is replaced.
func TestRedriveNamesTheOriginalSender(t *testing.T) {
	fromA := msg("a", "w1", 10)
	forged := withOriginalSender(withSender(msg("f", "w2", 10), senderB), senderA)
	resentBefore := withOriginalSender(withSender(msg("r", "w3", 10), operator), senderB)
	anonymous := withSender(msg("n", "w4", 10), "")
	api := newFakeSQS([]types.Message{fromA, forged, resentBefore, anonymous})
	api.nonBlocking = true

	if _, err := Redrive(context.Background(), api, "url/dlq.fifo", "url/in.fifo", operator, time.Now); err != nil {
		t.Fatal(err)
	}
	api.mu.Lock()
	in := api.lastReceive
	api.mu.Unlock()
	if !slices.Contains(in.MessageSystemAttributeNames, types.MessageSystemAttributeNameSenderId) ||
		!slices.Contains(in.MessageAttributeNames, AttrOriginalSenderID) || !slices.Contains(in.MessageAttributeNames, AttrFailureClass) {
		t.Fatalf("receive must ask for SenderId, %s and %s: %+v", AttrOriginalSenderID, AttrFailureClass, in)
	}
	want := map[string]string{"sqs-a": senderA, "sqs-f": senderB, "sqs-r": senderB, "sqs-n": ""}
	_, _, sent := api.snapshot()
	if len(sent) != len(want) {
		t.Fatalf("sent %d messages, want %d", len(sent), len(want))
	}
	for _, s := range sent {
		id, _, _ := strings.Cut(aws.ToString(s.MessageDeduplicationId), ":redrive:")
		got := aws.ToString(s.MessageAttributes[AttrOriginalSenderID].StringValue)
		if got != want[id] {
			t.Errorf("%s: originalSenderId %q, want %q", id, got, want[id])
		}
	}
}

func TestRedriveRequiresTheOperatorIdentity(t *testing.T) {
	api := newFakeSQS([]types.Message{msg("a", "w1", 10)})
	api.nonBlocking = true
	if _, err := Redrive(context.Background(), api, "url/dlq.fifo", "url/in.fifo", "", time.Now); err == nil {
		t.Fatal("redrive ran without knowing the operator's sender id")
	}
	if _, _, sent := api.snapshot(); len(sent) != 0 {
		t.Fatalf("sent %d messages", len(sent))
	}
}

package sqsconsumer

import (
	"context"
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
	res, err := Redrive(context.Background(), api, "url/dlq.fifo", "url/in.fifo", now)
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

package sqsconsumer

import (
	"context"
	"errors"
	"fmt"
	"strconv"
	"time"

	"github.com/aws/aws-sdk-go-v2/aws"
	"github.com/aws/aws-sdk-go-v2/service/sqs"
	"github.com/aws/aws-sdk-go-v2/service/sqs/types"
)

// RedriveAPI is the part of the SQS client the redrive tool uses.
type RedriveAPI interface {
	ReceiveMessage(ctx context.Context, in *sqs.ReceiveMessageInput, optFns ...func(*sqs.Options)) (*sqs.ReceiveMessageOutput, error)
	DeleteMessage(ctx context.Context, in *sqs.DeleteMessageInput, optFns ...func(*sqs.Options)) (*sqs.DeleteMessageOutput, error)
	SendMessage(ctx context.Context, in *sqs.SendMessageInput, optFns ...func(*sqs.Options)) (*sqs.SendMessageOutput, error)
}

// RedriveResult counts what one run did.
type RedriveResult struct {
	Redriven int
	// Skipped are failureClass=PERMANENT messages: they need a fix by the
	// producer and stay in the DLQ (ARCHITECTURE §10 step 2).
	Skipped int
}

// Redrive moves the messages the redrive policy put in dlqURL (transient
// attempts exhausted) back to queueURL with the same body and
// MessageGroupId, and MessageDeduplicationId <MessageId>:redrive:<unix
// seconds> so the source queue does not drop them as duplicates of the
// first delivery (ARCHITECTURE §10 step 3). Each is deleted from the DLQ
// only after the send succeeded. It returns when a receive comes back
// empty.
//
// The resend's SenderId is the operator's, so it names the original
// producer in AttrOriginalSenderID: the DLQ message's SenderId, or, for a
// message this operator (operatorSenderID) already resent once, the
// attribute it carries. Any other sender's attribute is overwritten.
//
// Skipped messages stay in flight until the DLQ's visibility timeout, so
// a later message of the same group (FIFO order) is only reachable when
// it came in the same batch; run the tool again after the timeout to
// reach the rest.
func Redrive(ctx context.Context, api RedriveAPI, dlqURL, queueURL, operatorSenderID string, now func() time.Time) (RedriveResult, error) {
	var res RedriveResult
	if principalID(operatorSenderID) == "" {
		return res, errors.New("the operator's sender id is required")
	}
	for {
		out, err := api.ReceiveMessage(ctx, &sqs.ReceiveMessageInput{
			QueueUrl:            aws.String(dlqURL),
			MaxNumberOfMessages: 10,
			WaitTimeSeconds:     2,
			MessageSystemAttributeNames: []types.MessageSystemAttributeName{
				types.MessageSystemAttributeNameMessageGroupId, types.MessageSystemAttributeNameSenderId,
			},
			MessageAttributeNames: []string{AttrFailureClass, AttrOriginalSenderID},
		})
		if err != nil {
			return res, fmt.Errorf("receive from DLQ: %w", err)
		}
		if len(out.Messages) == 0 {
			return res, nil
		}
		for _, m := range out.Messages {
			if a, ok := m.MessageAttributes[AttrFailureClass]; ok && aws.ToString(a.StringValue) == FailureClassPermanent {
				res.Skipped++
				continue
			}
			var attrs map[string]types.MessageAttributeValue
			if sender := originalSender(m, operatorSenderID); sender != "" {
				attrs = map[string]types.MessageAttributeValue{AttrOriginalSenderID: strAttr(sender)}
			}
			if _, err := api.SendMessage(ctx, &sqs.SendMessageInput{
				QueueUrl:               aws.String(queueURL),
				MessageBody:            m.Body,
				MessageGroupId:         aws.String(groupOf(m)),
				MessageDeduplicationId: aws.String(aws.ToString(m.MessageId) + ":redrive:" + strconv.FormatInt(now().Unix(), 10)),
				MessageAttributes:      attrs,
			}); err != nil {
				return res, fmt.Errorf("send %s to the source queue: %w", aws.ToString(m.MessageId), err)
			}
			if _, err := api.DeleteMessage(ctx, &sqs.DeleteMessageInput{QueueUrl: aws.String(dlqURL), ReceiptHandle: m.ReceiptHandle}); err != nil {
				// Sent but still in the DLQ: a second run would send it again,
				// which the inbox absorbs as a duplicate.
				return res, fmt.Errorf("delete %s from the DLQ: %w", aws.ToString(m.MessageId), err)
			}
			res.Redriven++
		}
	}
}

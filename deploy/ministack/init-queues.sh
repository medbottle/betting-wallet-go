#!/bin/sh
# MiniStack runs scripts in /etc/localstack/init/ready.d with `sh` once the
# gateway is up. Creates the queues from ARCHITECTURE.md §10 and writes a
# marker file the compose healthcheck waits for. Re-running is safe:
# CreateQueue with identical attributes is idempotent.
set -eu

export AWS_ACCESS_KEY_ID="${AWS_ACCESS_KEY_ID:-test}"
export AWS_SECRET_ACCESS_KEY="${AWS_SECRET_ACCESS_KEY:-test}"
REGION="${MINISTACK_REGION:-us-east-1}"
ENDPOINT="${SQS_INIT_ENDPOINT:-http://localhost:4566}"
MARKER=/tmp/wager-queues-ready
ATTRS=/tmp/wager-queue-attrs.json

rm -f "$MARKER"

sqs() { aws --endpoint-url "$ENDPOINT" --region "$REGION" sqs "$@"; }

i=0
until sqs list-queues >/dev/null 2>&1; do
  i=$((i + 1))
  if [ "$i" -ge 60 ]; then
    echo "init-queues: SQS endpoint not reachable" >&2
    exit 1
  fi
  sleep 1
done

# create_queue NAME VISIBILITY_SECONDS RETENTION_SECONDS [DLQ_ARN MAX_RECEIVE]
# Prints the queue ARN.
create_queue() {
  name=$1 visibility=$2 retention=$3
  redrive=""
  if [ $# -ge 5 ]; then
    redrive=",\"RedrivePolicy\":\"{\\\"deadLetterTargetArn\\\":\\\"$4\\\",\\\"maxReceiveCount\\\":\\\"$5\\\"}\""
  fi
  cat >"$ATTRS" <<EOF
{"FifoQueue":"true","ContentBasedDeduplication":"false","VisibilityTimeout":"$visibility","MessageRetentionPeriod":"$retention"$redrive}
EOF
  url=$(sqs create-queue --queue-name "$name" --attributes "file://$ATTRS" --query QueueUrl --output text)
  sqs get-queue-attributes --queue-url "$url" --attribute-names QueueArn --query Attributes.QueueArn --output text
}

FOUR_DAYS=345600
FOURTEEN_DAYS=1209600

# Inbound: wager-transactions.fifo -> wager-transactions-dlq.fifo after 10 receives.
in_dlq=$(create_queue wager-transactions-dlq.fifo 60 "$FOURTEEN_DAYS")
create_queue wager-transactions.fifo 60 "$FOUR_DAYS" "$in_dlq" 10 >/dev/null

# Outbound: wallet-events.fifo -> wallet-events-dlq.fifo for downstream consumers.
out_dlq=$(create_queue wallet-events-dlq.fifo 30 "$FOURTEEN_DAYS")
create_queue wallet-events.fifo 30 "$FOUR_DAYS" "$out_dlq" 10 >/dev/null

rm -f "$ATTRS"
echo "init-queues: queues ready"
sqs list-queues --output text
touch "$MARKER"

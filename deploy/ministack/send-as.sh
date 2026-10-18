#!/bin/sh
# Sends one message to the inbound queue as a provider's producer, the way
# a provider does (ARCHITECTURE §28): its IAM user assumes the provider's
# role, and the role sends. Runs inside the MiniStack container (docs/GUIA.md,
# "Inbound operations"):
#
#   send-as.sh PROVIDER MESSAGE_GROUP_ID MESSAGE_DEDUPLICATION_ID BODY
#
# BODY is the envelope JSON, or @FILE to read it from a file.
set -eu
[ $# -eq 4 ] || { echo "usage: send-as.sh PROVIDER MESSAGE_GROUP_ID MESSAGE_DEDUPLICATION_ID BODY|@FILE" >&2; exit 2; }
CREDENTIALS_FILE="${WAGER_CREDENTIALS_FILE:-/var/lib/wager-credentials/sqs-credentials.json}"
ENDPOINT="${SQS_INIT_ENDPOINT:-http://localhost:4566}"
REGION="${MINISTACK_REGION:-us-east-1}"
QUEUE_URL="${INBOUND_QUEUE_URL:-$ENDPOINT/000000000000/wager-transactions.fifo}"
body=$4
case "$body" in @*) body=$(cat "${body#@}") ;; esac

set -- "$1" "$2" "$3" $(python3 - "$CREDENTIALS_FILE" "$1" <<'PY'
import json, sys
k = json.load(open(sys.argv[1]))["providers"].get(sys.argv[2])
if not k:
    sys.exit("unknown provider " + sys.argv[2])
print(k["accessKeyId"], k["secretAccessKey"], k["roleArn"])
PY
)
set -- "$1" "$2" "$3" $(AWS_ACCESS_KEY_ID=$4 AWS_SECRET_ACCESS_KEY=$5 aws --endpoint-url "$ENDPOINT" --region "$REGION" \
  sts assume-role --role-arn "$6" --role-session-name "$1" \
  --query 'Credentials.[AccessKeyId,SecretAccessKey,SessionToken]' --output text)
AWS_ACCESS_KEY_ID=$4 AWS_SECRET_ACCESS_KEY=$5 AWS_SESSION_TOKEN=$6 aws --endpoint-url "$ENDPOINT" --region "$REGION" \
  sqs send-message --queue-url "$QUEUE_URL" --message-group-id "$2" --message-deduplication-id "$3" --message-body "$body"

#!/bin/sh
# MiniStack runs scripts in /etc/localstack/init/ready.d with `sh` once the
# gateway is up. MiniStack runs with AUTH=true (ARCHITECTURE §10, §28): this
# script creates the queues from ARCHITECTURE.md §10, then the broker
# identities, their least-privilege policies and the queue policies, writes
# the generated access keys to the credentials file the app, dlqredrive and
# the tests read, and finally writes the marker the compose healthcheck
# waits for.
#
# Account 000000000000 owns the queues. Its root key (`test`) is the
# emulator's administrator: only this script and the test harness use it.
# Each provider's producer and the redrive operator assume a role in their
# own account (provider-a 111111111111, provider-b 222222222222, operator
# 333333333333), because MiniStack reports the sender's account as SenderId.
#
#   init-queues.sh                         full provisioning (MiniStack start)
#   init-queues.sh queue-policies IN DLQ   apply the inbound and DLQ queue
#                                          policies to two more queues (tests)
set -eu

export AWS_ACCESS_KEY_ID=test
export AWS_SECRET_ACCESS_KEY=test
unset AWS_SESSION_TOKEN
REGION="${MINISTACK_REGION:-us-east-1}"
ENDPOINT="${SQS_INIT_ENDPOINT:-http://localhost:4566}"
CREDENTIALS_FILE="${WAGER_CREDENTIALS_FILE:-/var/lib/wager-credentials/sqs-credentials.json}"
MARKER=/tmp/wager-queues-ready
WORK=$(mktemp -d)
trap 'rm -rf "$WORK"' EXIT

PROVIDER_A_ACCOUNT=111111111111
PROVIDER_B_ACCOUNT=222222222222
OPERATOR_ACCOUNT=333333333333

aws_() { aws --endpoint-url "$ENDPOINT" --region "$REGION" "$@"; }
sqs() { aws_ sqs "$@"; }
# as_account ACCOUNT ...: IAM calls in another account, with its root key.
as_account() { acct=$1; shift; AWS_ACCESS_KEY_ID=$acct AWS_SECRET_ACCESS_KEY=test aws_ "$@"; }

# Identity policies name queues as arn:aws:sqs:*:*:<name>: MiniStack checks a
# role from another account against the queue ARN in the caller's account,
# so the account must be a wildcard here. The AWS definitions in
# deploy/aws/iam-policies.json name the exact ARNs. The -in-*, -dlq-* and
# -t-* names are the per-test queues of test/integration. MiniStack also
# checks SendMessageBatch as its own action (in AWS it is sqs:SendMessage),
# so the publisher's local policy names both.
INBOUND='"arn:aws:sqs:*:*:wager-transactions.fifo","arn:aws:sqs:*:*:wager-transactions-in-*"'
DLQ='"arn:aws:sqs:*:*:wager-transactions-dlq*"'
EVENTS='"arn:aws:sqs:*:*:wallet-events.fifo","arn:aws:sqs:*:*:wallet-events-t-*"'

role_arn() { echo "arn:aws:iam::$1:role/$2"; }
PROVIDER_A_ROLE=$(role_arn $PROVIDER_A_ACCOUNT wager-producer)
PROVIDER_B_ROLE=$(role_arn $PROVIDER_B_ACCOUNT wager-producer)
OPERATOR_ROLE=$(role_arn $OPERATOR_ACCOUNT wager-operator)

# set_queue_policy URL STATEMENTS: only the listed roles of other accounts
# get access; identities of the owner account need their own policy.
set_queue_policy() {
  arn=$(sqs get-queue-attributes --queue-url "$1" --attribute-names QueueArn --query Attributes.QueueArn --output text)
  policy="{\"Version\":\"2012-10-17\",\"Statement\":[$(echo "$2" | sed "s|QUEUE_ARN|$arn|g")]}"
  POLICY="$policy" python3 -c 'import json, os; print(json.dumps({"Policy": os.environ["POLICY"]}))' >"$WORK/policy.json"
  sqs set-queue-attributes --queue-url "$1" --attributes "file://$WORK/policy.json"
}

# Producers and the operator send to the inbound queue; only the operator
# reads and deletes from its DLQ.
queue_policies() {
  set_queue_policy "$1" "{\"Effect\":\"Allow\",\"Principal\":{\"AWS\":[\"$PROVIDER_A_ROLE\",\"$PROVIDER_B_ROLE\",\"$OPERATOR_ROLE\"]},\"Action\":[\"sqs:SendMessage\",\"sqs:GetQueueUrl\"],\"Resource\":\"QUEUE_ARN\"}"
  set_queue_policy "$2" "{\"Effect\":\"Allow\",\"Principal\":{\"AWS\":\"$OPERATOR_ROLE\"},\"Action\":[\"sqs:ReceiveMessage\",\"sqs:DeleteMessage\",\"sqs:ChangeMessageVisibility\",\"sqs:GetQueueUrl\"],\"Resource\":\"QUEUE_ARN\"}"
}

if [ "${1:-}" = "queue-policies" ]; then
  queue_policies "$2" "$3"
  exit 0
fi

rm -f "$MARKER"

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
# Prints the queue URL.
create_queue() {
  name=$1 visibility=$2 retention=$3
  redrive=""
  if [ $# -ge 5 ]; then
    redrive=",\"RedrivePolicy\":\"{\\\"deadLetterTargetArn\\\":\\\"$4\\\",\\\"maxReceiveCount\\\":\\\"$5\\\"}\""
  fi
  cat >"$WORK/attrs.json" <<EOF
{"FifoQueue":"true","ContentBasedDeduplication":"false","VisibilityTimeout":"$visibility","MessageRetentionPeriod":"$retention"$redrive}
EOF
  sqs create-queue --queue-name "$name" --attributes "file://$WORK/attrs.json" --query QueueUrl --output text
}
queue_arn() { sqs get-queue-attributes --queue-url "$1" --attribute-names QueueArn --query Attributes.QueueArn --output text; }

FOUR_DAYS=345600
FOURTEEN_DAYS=1209600

# Inbound: wager-transactions.fifo -> wager-transactions-dlq.fifo after 10 receives.
in_dlq=$(create_queue wager-transactions-dlq.fifo 60 "$FOURTEEN_DAYS")
in_queue=$(create_queue wager-transactions.fifo 60 "$FOUR_DAYS" "$(queue_arn "$in_dlq")" 10)

# Outbound: wallet-events.fifo -> wallet-events-dlq.fifo for downstream consumers.
out_dlq=$(create_queue wallet-events-dlq.fifo 30 "$FOURTEEN_DAYS")
create_queue wallet-events.fifo 30 "$FOUR_DAYS" "$(queue_arn "$out_dlq")" 10 >/dev/null

queue_policies "$in_queue" "$in_dlq"
echo "init-queues: queues ready"

# policy FILE STATEMENTS: an identity policy document.
policy() { echo "{\"Version\":\"2012-10-17\",\"Statement\":[$2]}" >"$WORK/$1"; }

# user NAME STATEMENTS: an IAM user of the owner account with its inline
# policy and a fresh access key, printed as "<key id> <secret>".
user() {
  aws_ iam create-user --user-name "$1" >/dev/null
  policy "$1.json" "$2"
  aws_ iam put-user-policy --user-name "$1" --policy-name "$1" --policy-document "file://$WORK/$1.json"
  aws_ iam create-access-key --user-name "$1" --query 'AccessKey.[AccessKeyId,SecretAccessKey]' --output text
}

# role ACCOUNT NAME TRUSTED_USER STATEMENTS: a role in ACCOUNT that only
# TRUSTED_USER (of the owner account) may assume.
role() {
  policy "trust-$1.json" "{\"Effect\":\"Allow\",\"Principal\":{\"AWS\":\"arn:aws:iam::000000000000:user/$3\"},\"Action\":\"sts:AssumeRole\"}"
  as_account "$1" iam create-role --role-name "$2" --assume-role-policy-document "file://$WORK/trust-$1.json" >/dev/null
  policy "role-$1.json" "$4"
  as_account "$1" iam put-role-policy --role-name "$2" --policy-name "$2" --policy-document "file://$WORK/role-$1.json"
}

assume() { echo "{\"Effect\":\"Allow\",\"Action\":\"sts:AssumeRole\",\"Resource\":\"$1\"}"; }

set -- $(user wallet-consumer "{\"Effect\":\"Allow\",\"Action\":[\"sqs:ReceiveMessage\",\"sqs:DeleteMessage\",\"sqs:ChangeMessageVisibility\",\"sqs:GetQueueUrl\",\"sqs:GetQueueAttributes\"],\"Resource\":[$INBOUND]},{\"Effect\":\"Allow\",\"Action\":[\"sqs:SendMessage\",\"sqs:GetQueueUrl\"],\"Resource\":[$DLQ]}")
CONSUMER_KEY=$1 CONSUMER_SECRET=$2
set -- $(user wallet-publisher "{\"Effect\":\"Allow\",\"Action\":[\"sqs:SendMessage\",\"sqs:SendMessageBatch\",\"sqs:GetQueueUrl\",\"sqs:GetQueueAttributes\"],\"Resource\":[$EVENTS]}")
PUBLISHER_KEY=$1 PUBLISHER_SECRET=$2
set -- $(user provider-a-producer "$(assume "$PROVIDER_A_ROLE")")
PROVIDER_A_KEY=$1 PROVIDER_A_SECRET=$2
set -- $(user provider-b-producer "$(assume "$PROVIDER_B_ROLE")")
PROVIDER_B_KEY=$1 PROVIDER_B_SECRET=$2
set -- $(user wallet-operator "$(assume "$OPERATOR_ROLE")")
OPERATOR_KEY=$1 OPERATOR_SECRET=$2

PRODUCER="{\"Effect\":\"Allow\",\"Action\":[\"sqs:SendMessage\",\"sqs:GetQueueUrl\"],\"Resource\":[$INBOUND]}"
role $PROVIDER_A_ACCOUNT wager-producer provider-a-producer "$PRODUCER"
role $PROVIDER_B_ACCOUNT wager-producer provider-b-producer "$PRODUCER"
role $OPERATOR_ACCOUNT wager-operator wallet-operator "{\"Effect\":\"Allow\",\"Action\":[\"sqs:ReceiveMessage\",\"sqs:DeleteMessage\",\"sqs:ChangeMessageVisibility\",\"sqs:GetQueueUrl\"],\"Resource\":[$DLQ]},$PRODUCER"

mkdir -p "$(dirname "$CREDENTIALS_FILE")"
export CONSUMER_KEY CONSUMER_SECRET PUBLISHER_KEY PUBLISHER_SECRET PROVIDER_A_KEY PROVIDER_A_SECRET \
  PROVIDER_B_KEY PROVIDER_B_SECRET OPERATOR_KEY OPERATOR_SECRET PROVIDER_A_ROLE PROVIDER_B_ROLE OPERATOR_ROLE
python3 - "$CREDENTIALS_FILE" <<'EOF'
import json, os, sys
e = os.environ
def key(prefix, role=None):
    k = {"accessKeyId": e[prefix + "_KEY"], "secretAccessKey": e[prefix + "_SECRET"]}
    if role:
        k["roleArn"] = e[role]
    return k
creds = {
    "consumer": key("CONSUMER"),
    "publisher": key("PUBLISHER"),
    "operator": key("OPERATOR", "OPERATOR_ROLE"),
    "providers": {
        "provider-a": key("PROVIDER_A", "PROVIDER_A_ROLE"),
        "provider-b": key("PROVIDER_B", "PROVIDER_B_ROLE"),
    },
}
tmp = sys.argv[1] + ".tmp"
with open(tmp, "w") as f:
    json.dump(creds, f, indent=2)
os.chmod(tmp, 0o644)
os.replace(tmp, sys.argv[1])
EOF
echo "init-queues: identities ready ($CREDENTIALS_FILE)"
sqs list-queues --output text
touch "$MARKER"

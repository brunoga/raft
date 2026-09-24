#!/usr/bin/env bash
# clusterlib.sh — the scaffolding every example's cluster.sh shares.
#
# The scripts were each written to the same shape and had drifted: different
# banners, different readiness checks, and one that exited before printing
# anything because of a `set -e` interaction described under count_matches.
# This is that shape, in one place, so "they all do the same thing" is a fact
# rather than an intention.
#
# A cluster.sh sources this, then:
#
#   cluster_require_bash
#   cluster_start "n1" "$BINARY" --id n1 ...     # records the PID
#   cluster_wait_ready "3 nodes to elect a leader" 30 is_ready
#   cluster_footer
#
# It is sourced rather than copied because these scripts already only work
# inside a checkout -- they build from $REPO_ROOT -- so there is nothing to
# be gained by making each one repeat this and drift again.

# Data directory and process table. A script sets DATA_ROOT before sourcing,
# or right after; the functions here read it when they run, not now.
: "${DATA_ROOT:?clusterlib: set DATA_ROOT before using these helpers}"

CLUSTER_PIDS=()
CLUSTER_NAMES=()

# count_matches PATTERN — count matching lines on stdin, and never fail.
#
# grep exits 1 when it matches nothing. Under `set -o pipefail` that becomes
# the exit status of the whole pipeline, and in `n=$(... | grep ... | wc -l)`
# it becomes the status of the assignment -- which `set -e` treats as a fatal
# error. So a readiness loop polling for something that is not there yet
# kills the script on its first pass, before printing a single dot. That is
# not hypothetical: it is what shipped in the tenants example.
count_matches() {
    grep -c -- "$1" 2>/dev/null || true
}

# count_occurrences PATTERN — count matches including several per line.
count_occurrences() {
    local n
    n=$(grep -o -- "$1" 2>/dev/null | wc -l) || n=0
    printf '%s' "${n:-0}"
}

# cluster_start NAME COMMAND [ARGS...] — run a node in the background, with
# its output in $DATA_ROOT/NAME.log, and remember it so the readiness check
# can notice it dying and cleanup can stop it.
cluster_start() {
    local name="$1"
    shift
    echo "==> Starting $name..."
    "$@" >"$DATA_ROOT/$name.log" 2>&1 &
    CLUSTER_PIDS+=($!)
    CLUSTER_NAMES+=("$name")
}

# cluster_stop — stop every node started through cluster_start. Idempotent,
# because it runs from both the signal trap and the failure path.
cluster_stop() {
    local pid
    for pid in "${CLUSTER_PIDS[@]:-}"; do
        kill "$pid" 2>/dev/null || true
    done
    wait 2>/dev/null || true
    CLUSTER_PIDS=()
}

# cluster_dead_node — print the name of the first node that is no longer
# running, or nothing if they are all up.
cluster_dead_node() {
    local i
    for i in "${!CLUSTER_PIDS[@]}"; do
        if ! kill -0 "${CLUSTER_PIDS[$i]}" 2>/dev/null; then
            printf '%s' "${CLUSTER_NAMES[$i]}"
            return 0
        fi
    done
    return 1
}

# cluster_wait_ready LABEL TIMEOUT PREDICATE — poll until PREDICATE succeeds.
#
# Returns 0 when it does. Returns 1 when a node died or the timeout expired,
# having first said which and shown the end of the relevant log -- because
# the failure this replaces printed dots for a minute and then a table of
# zeroes, which told nobody that nothing was running.
cluster_wait_ready() {
    local label="$1" timeout="$2" predicate="$3"
    local deadline=$((SECONDS + timeout))

    printf '==> Waiting for %s (up to %ds)...' "$label" "$timeout"
    while [[ $SECONDS -lt $deadline ]]; do
        local dead
        if dead=$(cluster_dead_node); then
            printf '\n'
            echo "==> Node $dead exited. The end of $DATA_ROOT/$dead.log:"
            sed 's/^/    /' "$DATA_ROOT/$dead.log" 2>/dev/null | tail -20
            return 1
        fi
        if "$predicate"; then
            printf ' done.\n'
            return 0
        fi
        printf '.'
        sleep 0.3
    done

    printf '\n'
    echo "==> Not ready after ${timeout}s. The end of each log:"
    local name
    for name in "${CLUSTER_NAMES[@]:-}"; do
        echo "--- $name ---"
        sed 's/^/    /' "$DATA_ROOT/$name.log" 2>/dev/null | tail -10
    done
    return 1
}

# cluster_give_up — stop everything and leave, for when readiness failed.
cluster_give_up() {
    echo ""
    echo "==> The cluster did not come up. Stopping what is running."
    cluster_stop
    rm -rf "$DATA_ROOT"
    exit 1
}

# ---- The smoke test -------------------------------------------------------
#
# A script registers its steps, and the same list is either printed for the
# reader to copy or run for them with an explanation of each:
#
#   cluster_smoke "Write a value" \
#       "curl -sS ..." \
#       "The write goes to whichever node was asked and is redirected to the
# leader, which is why -L is here and why any endpoint works."
#
# One list rather than two is the point. These commands used to be echoed as
# text nobody executed, which is how a printed command survives the endpoint
# it calls being renamed. In --demo they run, so a stale one is a failure
# rather than a surprise for whoever copies it.
CLUSTER_SMOKE_DESCS=()
CLUSTER_SMOKE_CMDS=()
CLUSTER_SMOKE_NOTES=()
CLUSTER_DEMO_INTRO=""

# cluster_demo_intro TEXT — what this example is for, shown once at the top
# of a demonstration. Without it a demo is a wall of curl output that says
# what happened and never what it meant.
cluster_demo_intro() {
    CLUSTER_DEMO_INTRO="$1"
}

# cluster_smoke DESC CMD [NOTE] — register a step. NOTE is prose explaining
# what the step shows; it is printed in --demo and left out of the copyable
# list, where the command and its title are the whole point.
#
# Quoting CMD decides what the reader sees, and the rule is which side of the
# script a name lives on:
#
#   "..."  for this script's own plumbing -- a binary path, a group count, an
#          endpoint list. Double quotes expand it here, so both the printed
#          command and the demonstration show something that can be pasted
#          into a shell that has never heard of $CTL.
#
#   '...'  for anything the command itself creates: a loop variable, or a
#          value an earlier step assigned, such as the ETag configsvc reads
#          and then writes back. There the variable *is* the thing to copy,
#          and expanding it here would print an empty string.
cluster_smoke() {
    CLUSTER_SMOKE_DESCS+=("$1")
    CLUSTER_SMOKE_CMDS+=("$2")
    CLUSTER_SMOKE_NOTES+=("${3:-}")
}

# cluster_smoke_print — show the steps without running them.
cluster_smoke_print() {
    echo "Quick smoke-test:"
    local i
    for i in "${!CLUSTER_SMOKE_CMDS[@]}"; do
        echo "  # ${CLUSTER_SMOKE_DESCS[$i]}"
        echo "  ${CLUSTER_SMOKE_CMDS[$i]}"
    done
}

# cluster_indent PREFIX TEXT — print TEXT with every line prefixed, so a note
# written across several lines in the script reads as a paragraph here.
cluster_indent() {
    local prefix="$1" text="$2"
    printf '%s\n' "$text" | sed "s/^[[:space:]]*/$prefix/"
}

# cluster_smoke_run — walk the steps, saying what each one demonstrates
# before running it and showing what it said.
cluster_smoke_run() {
    echo "============================================================"
    echo " Demonstration"
    echo "============================================================"
    if [[ -n "$CLUSTER_DEMO_INTRO" ]]; then
        echo ""
        cluster_indent " " "$CLUSTER_DEMO_INTRO"
    fi

    local i n=${#CLUSTER_SMOKE_CMDS[@]}
    for i in "${!CLUSTER_SMOKE_CMDS[@]}"; do
        echo ""
        echo "------------------------------------------------------------"
        printf ' Step %d of %d: %s\n' "$((i + 1))" "$n" "${CLUSTER_SMOKE_DESCS[$i]}"
        echo "------------------------------------------------------------"
        if [[ -n "${CLUSTER_SMOKE_NOTES[$i]}" ]]; then
            cluster_indent " " "${CLUSTER_SMOKE_NOTES[$i]}"
            echo ""
        fi
        echo "\$ ${CLUSTER_SMOKE_CMDS[$i]}"
        if ! eval "${CLUSTER_SMOKE_CMDS[$i]}"; then
            echo ""
            echo "!!! This step failed: ${CLUSTER_SMOKE_DESCS[$i]}"
            return 1
        fi
        echo ""
    done

    echo "============================================================"
    echo " Every step succeeded."
    echo "============================================================"
}

# ---- Arguments and the two modes ------------------------------------------

CLUSTER_DEMO=0
# CLUSTER_ARGS holds the arguments this library did not consume, for the
# scripts that forward flags to the binary they start. They opt in with
# --passthrough; everywhere else an unrecognised argument is a mistake worth
# reporting rather than something to hand to a node, which is how --demo once
# reached a node as an unknown flag and killed it at startup.
CLUSTER_ARGS=()

cluster_parse_args() {
    local passthrough=0
    if [[ "${1:-}" == "--passthrough" ]]; then
        passthrough=1
        shift
    fi
    CLUSTER_ARGS=()
    while [[ $# -gt 0 ]]; do
        case "$1" in
        --demo)
            CLUSTER_DEMO=1
            ;;
        -h | --help)
            cat <<USAGE
usage: $(basename "$0") [--demo]

  (no flags)  start the cluster, print how to exercise it, and stay up until
              Ctrl-C.
  --demo      start the cluster, run the smoke test and show its output, then
              stop everything and exit. Non-zero if any step fails, so this is
              usable as an end-to-end check.
USAGE
            exit 0
            ;;
        *)
            if [[ $passthrough -eq 1 ]]; then
                CLUSTER_ARGS+=("$1")
            else
                echo "unknown argument: $1" >&2
                echo "try --help" >&2
                exit 2
            fi
            ;;
        esac
        shift
    done
}

# cluster_footer — the last thing every script does. In the default mode it
# prints the smoke test and waits; with --demo it runs it and leaves.
cluster_footer() {
    if [[ $CLUSTER_DEMO -eq 1 ]]; then
        local status=0
        cluster_smoke_run || status=1
        echo ""
        echo "==> Stopping nodes..."
        cluster_stop
        rm -rf "$DATA_ROOT"
        exit "$status"
    fi

    cluster_smoke_print
    echo ""
    echo "Logs: $DATA_ROOT/"
    echo "Press Ctrl-C to stop."
    echo "Run with --demo to exercise all of the above automatically."
    echo ""
    wait
}

//go:build integration

package integration

import (
	"testing"

	"foci/internal/testharness"
)

// Cron / scheduled-trigger tests for the L2 layer.
//
// foci has three families of "fires-on-its-own" machinery, all of which
// reach foci's agent dispatch path without a Telegram message arriving:
//
//  1. Internal periodic timers in internal/periodic — keepalive,
//     background work, reflection, consolidation. Each ticks every 30s
//     and decides whether to fire based on config interval, in-flight
//     state, mana/rate-limit gating, etc.
//
//  2. Wake reminders via the `remind` tool (internal/tools/remind.go +
//     buildWakeScheduler in cmd/foci-gw/agents_notify.go). An agent
//     schedules a deferred message to itself, which fires via a
//     time.After goroutine and lands as an injected turn.
//
//  3. External system cron entries provisioned from
//     shared/crontab.template via internal/provision/crontab.go. These
//     shell out to `foci branch --oneshot`, so L2 can drive them by
//     invoking the same CLI path.
//
// Per the README, L2 stays away from real time-warp: tests configure
// short intervals (via the config writer) or pre-seed in-memory state
// so the next tick fires. Tests requiring hour-scale waits or true
// wall-clock fidelity belong in L3.

// TestL2_Cron_KeepaliveFiresAtConfiguredInterval proves the keepalive
// timer in internal/periodic dispatches a branch session with the
// keepalive prompt when the cache-age threshold is crossed. With a
// sub-minute interval set in the test config and the cache age forced
// past it, the next 30s tick should call branchFn("keepalive", ...),
// which routes through Agent.Branch and spawns cc-stub in the agent's
// workdir. The recorder should show an extra invocation for the agent
// without any Telegram update having been pushed.
func TestL2_Cron_KeepaliveFiresAtConfiguredInterval(t *testing.T) {
	t.Skip("not yet implemented")
	_ = testharness.HarnessOptions{}
}

// TestL2_Cron_KeepaliveSkippedWhenCachingUnavailable proves the
// keepalive guard at the top of maybeKeepalive: if the provider client
// reports caching is unavailable (or the explicit cachingOverride is
// false), no branch is dispatched even after the interval elapses. The
// stub backend exposes no real caching, so configuring keepalive
// without a caching-capable model should leave the recorder free of
// keepalive invocations across multiple ticks.
func TestL2_Cron_KeepaliveSkippedWhenCachingUnavailable(t *testing.T) {
	t.Skip("not yet implemented")
	_ = testharness.HarnessOptions{}
}

// TestL2_Cron_KeepaliveSkippedWhenTurnInFlight proves the in-flight
// guard added by TODO #760: if a user turn is mid-flight on the parent
// session when the keepalive tick fires, the runner defers rather than
// queueing the keepalive prompt as a SourceUser follow-up. The test
// holds a turn open (cc-stub script with a long-running Bash tool_use)
// while the keepalive interval elapses, then verifies no keepalive
// invocation was recorded until after the held turn completes.
func TestL2_Cron_KeepaliveSkippedWhenTurnInFlight(t *testing.T) {
	t.Skip("not yet implemented")
	_ = testharness.HarnessOptions{}
}

// TestL2_Cron_KeepaliveDoesNotReplyToTelegram proves the keepalive
// prompt "[KEEPALIVE] ... respond with [[NO_RESPONSE]]" is treated as
// internal: even though cc-stub by default echoes the user text, the
// runner branches with noCompact=true into a fresh session and the
// branch egress path must not produce a sendMessage to the Telegram
// stub. The assertion is the absence of any user-visible message for
// the keepalive prompt body in the Telegram stub's call log.
func TestL2_Cron_KeepaliveDoesNotReplyToTelegram(t *testing.T) {
	t.Skip("not yet implemented")
	_ = testharness.HarnessOptions{}
}

// TestL2_Cron_BackgroundFiresWhenIdleWithOpenTodos proves the
// background-work scheduler in internal/periodic fires when (a) the
// configured idle interval has elapsed since last interaction, (b)
// there's at least one open todo tagged "background", and (c) mana /
// rate-limit gating allows. The test seeds a background todo via the
// todo store path, leaves the agent idle past the interval, and
// asserts a recorder entry with the background prompt appears.
func TestL2_Cron_BackgroundFiresWhenIdleWithOpenTodos(t *testing.T) {
	t.Skip("not yet implemented")
	_ = testharness.HarnessOptions{}
}

// TestL2_Cron_BackgroundSkippedWithNoOpenTodos proves the explicit
// "no open background todos" skip branch in maybeBackgroundWork: even
// when idle past the interval, with no qualifying todos the scheduler
// must not dispatch. Asserts the recorder shows no background-prompt
// invocation across multiple ticks when the todo store is empty.
func TestL2_Cron_BackgroundSkippedWithNoOpenTodos(t *testing.T) {
	t.Skip("not yet implemented")
	_ = testharness.HarnessOptions{}
}

// TestL2_Cron_BackgroundCooldownPreventsSelfChaining proves the
// post-completion cooldown enforced by maybeBackgroundWork: the
// scheduler refuses to start another background session within the
// configured interval of the last one ending. This is the regression
// net for the orphaned-tmux-child accumulation bug — without the
// cooldown, each completed background session would immediately trigger
// the next. The test runs one background session, then verifies the
// next tick declines despite open todos remaining.
func TestL2_Cron_BackgroundCooldownPreventsSelfChaining(t *testing.T) {
	t.Skip("not yet implemented")
	_ = testharness.HarnessOptions{}
}

// TestL2_Cron_BackgroundSkippedWhileActiveTmuxWatches proves the
// hasActiveWorkFn gate: when the agent has live tmux watches (the
// callback returns >0), the background scheduler defers regardless of
// idle time or open todos. Test wires a stub HasActiveWorkFn returning
// 1 and confirms no dispatch occurs.
func TestL2_Cron_BackgroundSkippedWhileActiveTmuxWatches(t *testing.T) {
	t.Skip("not yet implemented")
	_ = testharness.HarnessOptions{}
}

// TestL2_Cron_ReflectionFiresOnInterval proves the periodic reflection
// pass dispatches a branch session with the reflection prompt for each
// session reported as needing reflection by SessionIndex. After a
// Telegram message creates an active session, a configured short
// reflection interval should cause the next eligible tick to branch
// from that session key and run the reflection prompt in cc-stub. The
// assertion is a recorder entry whose text prefix matches the
// reflection prompt header.
func TestL2_Cron_ReflectionFiresOnInterval(t *testing.T) {
	t.Skip("not yet implemented")
	_ = testharness.HarnessOptions{}
}

// TestL2_Cron_ReflectionStampPreventsImmediateRefire proves the
// SessionIndex.StampReflection write inside maybeReflection: once a
// session has been reflected on, subsequent ticks within the configured
// interval skip it (the "no sessions need reflection" path). Asserts at
// most one reflection invocation per session across a window of
// multiple ticks.
func TestL2_Cron_ReflectionStampPreventsImmediateRefire(t *testing.T) {
	t.Skip("not yet implemented")
	_ = testharness.HarnessOptions{}
}

// TestL2_Cron_ReflectionDeferredWhenAllSessionsBusy proves the
// in-flight filter in maybeReflection (TODO #760): when every session
// flagged as needing reflection has an active turn, the reflection
// pass logs "all candidate sessions have in-flight turns" and skips.
// Test holds a turn open via cc-stub and asserts no reflection
// recorder entry appears for that session until the turn completes.
func TestL2_Cron_ReflectionDeferredWhenAllSessionsBusy(t *testing.T) {
	t.Skip("not yet implemented")
	_ = testharness.HarnessOptions{}
}

// TestL2_Cron_ReflectionDisabledWhenIntervalEnabledFalse proves the
// IntervalEnabled=false config path: even with eligible sessions and a
// short interval, the scheduler returns immediately without
// dispatching. Asserts the recorder shows zero reflection invocations.
func TestL2_Cron_ReflectionDisabledWhenIntervalEnabledFalse(t *testing.T) {
	t.Skip("not yet implemented")
	_ = testharness.HarnessOptions{}
}

// TestL2_Cron_ConsolidationFiresOnLongerInterval proves consolidation
// dispatches via RunOnceFunc (when set) or branchFn (otherwise) with
// the memory-consolidation prompt. With a short ConsolidationInterval
// and recent interaction, the scheduler should call the RunOnce path
// once per interval. Asserts on the recorder for a consolidation
// prompt invocation in the agent's workdir.
func TestL2_Cron_ConsolidationFiresOnLongerInterval(t *testing.T) {
	t.Skip("not yet implemented")
	_ = testharness.HarnessOptions{}
}

// TestL2_Cron_ConsolidationSkippedWhileReflectionRunning proves the
// "reflection running" guard in maybeConsolidation: if a reflection
// pass is in flight when the consolidation tick fires, consolidation
// defers. Test forces overlap by scripting cc-stub to hang during
// reflection and asserts consolidation only runs after reflection
// completes.
func TestL2_Cron_ConsolidationSkippedWhileReflectionRunning(t *testing.T) {
	t.Skip("not yet implemented")
	_ = testharness.HarnessOptions{}
}

// TestL2_Cron_ConsolidationTimestampPersistsAcrossRestart proves the
// SessionIndex.SetAgentMetadata("consolidation_last", ...) write at
// the end of maybeConsolidation survives a foci-gw restart: a second
// harness pointed at the same DataDir should pick up the timestamp
// and refuse to fire consolidation again within the configured
// interval. Asserts at most one consolidation invocation across both
// processes.
func TestL2_Cron_ConsolidationTimestampPersistsAcrossRestart(t *testing.T) {
	t.Skip("not yet implemented")
	_ = testharness.HarnessOptions{}
}

// TestL2_Cron_WakeReminderFiresAfterDelay proves the remind tool's
// wake path end-to-end via the exec bridge: scripted cc-stub runs
// `foci_remind --text "wake me" --when 2s --wake true`, which writes
// a row to ReminderStore and spawns a time.After goroutine. After the
// delay, buildWakeScheduler.wakeScheduleFn injects a SCHEDULED WAKE
// turn into the originating session. Asserts the recorder shows a
// user_message containing the SCHEDULED WAKE header plus the original
// reminder text.
func TestL2_Cron_WakeReminderFiresAfterDelay(t *testing.T) {
	t.Skip("not yet implemented")
	_ = testharness.HarnessOptions{}
}

// TestL2_Cron_WakeReminderSurvivesRestart proves
// ReminderStore.PendingWakes + buildWakeScheduler's restore loop:
// when a wake is scheduled, foci-gw is stopped before it fires, and a
// fresh foci-gw is started against the same DataDir, the pending wake
// should be re-scheduled and still fire. The injected turn lands in
// the second process's recorder.
func TestL2_Cron_WakeReminderSurvivesRestart(t *testing.T) {
	t.Skip("not yet implemented")
	_ = testharness.HarnessOptions{}
}

// TestL2_Cron_WakeReminderRejectsNegativeDelay proves the
// resolveWakeDuration validation: when the cc-stub script invokes
// `foci_remind ... --when -5s --wake true`, the tool returns an error
// without writing to ReminderStore or scheduling a goroutine. Asserts
// the recorder shows no SCHEDULED WAKE injection and the tool exec
// produced a non-zero result.
func TestL2_Cron_WakeReminderRejectsNegativeDelay(t *testing.T) {
	t.Skip("not yet implemented")
	_ = testharness.HarnessOptions{}
}

// TestL2_Cron_WakeReminderRejectsUnparseableWhen proves the catch-all
// branch in resolveWakeDuration: an unrecognised `when` value (not a
// Go duration, not RFC3339, not a YYYY-MM-DD date, not a known tag)
// fails the tool call. Asserts the tool returns "cannot parse when ..."
// and no DB row or goroutine is created.
func TestL2_Cron_WakeReminderRejectsUnparseableWhen(t *testing.T) {
	t.Skip("not yet implemented")
	_ = testharness.HarnessOptions{}
}

// TestL2_Cron_WakeReminderRoutesToOriginatingSession proves the
// session-key threading in wakeScheduleFn: when the reminder is
// scheduled from agent A's session S1, the fired wake must inject into
// S1 specifically — not into "the most recent session on A" if A has
// since served other sessions. Test creates two sessions on one agent,
// schedules a wake from the older one, drives traffic to the newer
// one, and asserts the wake lands in the older session's recorder
// entries (matched by session_id).
func TestL2_Cron_WakeReminderRoutesToOriginatingSession(t *testing.T) {
	t.Skip("not yet implemented")
	_ = testharness.HarnessOptions{}
}

// TestL2_Cron_WakeReminderWaitsForActiveTurn proves the
// IsProcessing() spin in wakeScheduleFn: when the wake delay elapses
// while a user turn is still being processed on the target session,
// the injection waits rather than racing the active turn. Test
// schedules a 1s wake, immediately sends a Telegram message that
// scripts cc-stub to hang for 3s, and asserts the SCHEDULED WAKE
// user_message appears only after the held turn's user_message.
func TestL2_Cron_WakeReminderWaitsForActiveTurn(t *testing.T) {
	t.Skip("not yet implemented")
	_ = testharness.HarnessOptions{}
}

// TestL2_Cron_RemindToolUnavailableWithoutWakeFn proves the negative
// path documented in agents_delegated_test.go: when reminderStore is
// nil (reminders disabled), buildWakeScheduler returns nil and the
// remind tool must not be registered. Scripted cc-stub running
// `foci_remind ...` should see an "unknown tool" / missing exec
// bridge function rather than a successful schedule. Asserts the
// recorder shows no SCHEDULED WAKE injection regardless of how long
// the test waits.
func TestL2_Cron_RemindToolUnavailableWithoutWakeFn(t *testing.T) {
	t.Skip("not yet implemented")
	_ = testharness.HarnessOptions{}
}

// TestL2_Cron_GenerateCrontabRendersAgentPlaceholders proves the
// crontab generator path that foci-install runs: with the agent spec
// + shared/crontab.template, GenerateCrontab returns one line per
// non-comment template entry with AGENT_NAME / HOMEDIR / WORKSPACE
// substituted. L2 drives this via the foci CLI subcommand (no real
// crontab is touched — RunCrontabCmd is overridden in tests).
// Asserts the rendered lines contain the agent id and the agent's
// resolved workspace path.
func TestL2_Cron_GenerateCrontabRendersAgentPlaceholders(t *testing.T) {
	t.Skip("not yet implemented")
	_ = testharness.HarnessOptions{}
}

// TestL2_Cron_GenerateCrontabStaggersMultipleAgents proves
// StaggerCrontabLine: when GenerateCrontab is called with
// existingAgentCount>0, the absolute minute fields are offset by
// existingAgentCount*3 (mod 60) while */N interval fields are left
// alone. Test generates entries for three agents and asserts the
// minute fields differ by the stagger offset.
func TestL2_Cron_GenerateCrontabStaggersMultipleAgents(t *testing.T) {
	t.Skip("not yet implemented")
	_ = testharness.HarnessOptions{}
}

// TestL2_Cron_BranchOneshotEndToEnd proves the external-cron contract
// rendered into the crontab template: `foci branch --oneshot -a <agent>
// -mf <prompt-file>` reaches foci-gw via the local unix socket, the
// gateway dispatches a branch session on the agent, and cc-stub
// records an invocation with the prompt body. This is the L2 hook
// for any system-cron-driven workflow (the weekly character review
// in the template, future scheduled jobs).
func TestL2_Cron_BranchOneshotEndToEnd(t *testing.T) {
	t.Skip("not yet implemented")
	_ = testharness.HarnessOptions{}
}

// TestL2_Cron_BranchOneshotRejectsUnknownAgent proves the gateway
// rejects a `foci branch --oneshot -a <missing>` call with a clear
// error rather than silently dropping it. Asserts the CLI exits
// non-zero with a message naming the missing agent, and no recorder
// invocation is created.
func TestL2_Cron_BranchOneshotRejectsUnknownAgent(t *testing.T) {
	t.Skip("not yet implemented")
	_ = testharness.HarnessOptions{}
}

// TestL2_Cron_BranchOneshotMalformedPromptFile proves the CLI surfaces
// a file-not-found / unreadable error when -mf points at a missing
// path, rather than dispatching with an empty prompt. Asserts the CLI
// exits non-zero with a path-related error and the recorder shows no
// branch invocation.
func TestL2_Cron_BranchOneshotMalformedPromptFile(t *testing.T) {
	t.Skip("not yet implemented")
	_ = testharness.HarnessOptions{}
}

// TestL2_Cron_RateLimitGateBlocksAllSchedulers proves the shared
// canFireFn / sessionKeyFn check at the top of every scheduler
// (background, reflection, consolidation): when the rate-limit/mana
// callback returns canFire=false with a reason, none of the three
// schedulers dispatch. Test wires a stub canFireFn returning false
// and confirms zero invocations across multiple ticks despite all
// other conditions being met.
func TestL2_Cron_RateLimitGateBlocksAllSchedulers(t *testing.T) {
	t.Skip("not yet implemented")
	_ = testharness.HarnessOptions{}
}

// TestL2_Cron_IncomingMessageDuringKeepaliveQueues proves the
// interaction between an ingress Telegram message and an in-flight
// keepalive branch: the user's message should not be dropped, and
// must process as a normal turn once the keepalive branch completes.
// This is the cron-side companion to the message-queueing behaviour
// documented in MEMORY for permission waits. Asserts both the
// keepalive recorder entry AND the user-message recorder entry are
// present, in that order.
func TestL2_Cron_IncomingMessageDuringKeepaliveQueues(t *testing.T) {
	t.Skip("not yet implemented")
	_ = testharness.HarnessOptions{}
}

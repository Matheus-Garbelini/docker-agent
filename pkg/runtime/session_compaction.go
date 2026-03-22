package runtime

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"log/slog"
	"os/exec"
	"strings"
	"time"

	"github.com/docker/docker-agent/pkg/agent"
	"github.com/docker/docker-agent/pkg/chat"
	"github.com/docker/docker-agent/pkg/compaction"
	"github.com/docker/docker-agent/pkg/config/latest"
	"github.com/docker/docker-agent/pkg/model/provider"
	"github.com/docker/docker-agent/pkg/model/provider/options"
	"github.com/docker/docker-agent/pkg/session"
	"github.com/docker/docker-agent/pkg/shellpath"
	"github.com/docker/docker-agent/pkg/team"
)

const compactionScriptTimeout = 30 * time.Second

type compactionOutcome struct {
	summary     string
	messages    []chat.Message
	inputTokens int64
	cost        float64
}

// runSummarization sends the prepared messages through a one-shot runtime
// and returns the model's summary together with the output token count and
// cost. The runtime is created with compaction disabled so it cannot recurse.
func runSummarization(ctx context.Context, model provider.Provider, messages []chat.Message) (compaction.Result, error) {
	summaryModel := provider.CloneWithOptions(ctx, model, options.WithStructuredOutput(nil))
	root := agent.New("root", compaction.SystemPrompt, agent.WithModel(summaryModel))
	t := team.New(team.WithAgents(root))

	sess := session.New()
	sess.Title = "Generating summary..."
	for _, msg := range messages {
		sess.AddMessage(&session.Message{Message: msg})
	}

	rt, err := New(t, WithSessionCompaction(false))
	if err != nil {
		return compaction.Result{}, err
	}
	if _, err = rt.Run(ctx, sess); err != nil {
		return compaction.Result{}, err
	}

	return compaction.Result{
		Summary:     sess.GetLastAssistantMessageContent(),
		InputTokens: sess.OutputTokens,
		Cost:        sess.TotalCost(),
	}, nil
}

func estimateMessagesTokens(messages []chat.Message) int64 {
	var total int64
	for i := range messages {
		total += compaction.EstimateMessageTokens(&messages[i])
	}
	return total
}

func normalizeCompactionMessages(messages []chat.Message) ([]chat.Message, error) {
	toolCallIDs := make(map[string]struct{})
	normalized := make([]chat.Message, 0, len(messages))

	for _, msg := range messages {
		switch msg.Role {
		case chat.MessageRoleSystem:
			continue
		case chat.MessageRoleUser, chat.MessageRoleAssistant, chat.MessageRoleTool:
		default:
			return nil, fmt.Errorf("invalid compaction message role: %q", msg.Role)
		}

		if msg.Role == chat.MessageRoleAssistant {
			for _, tc := range msg.ToolCalls {
				if tc.ID != "" {
					toolCallIDs[tc.ID] = struct{}{}
				}
			}
		}

		normalized = append(normalized, msg)
	}

	for _, msg := range normalized {
		if msg.Role != chat.MessageRoleTool || msg.ToolCallID == "" {
			continue
		}
		if _, ok := toolCallIDs[msg.ToolCallID]; !ok {
			return nil, fmt.Errorf("tool result %q has no matching tool call in compaction output", msg.ToolCallID)
		}
	}

	if normalized == nil {
		return []chat.Message{}, nil
	}

	return normalized, nil
}

func (r *LocalRuntime) runCompactionScript(ctx context.Context, sess *session.Session, script string, messages []chat.Message, additionalPrompt string) (compactionOutcome, error) {
	inputMessages := append([]chat.Message(nil), messages...)
	if additionalPrompt != "" {
		inputMessages = append(inputMessages, chat.Message{
			Role:      chat.MessageRoleUser,
			Content:   "Additional instructions from user: " + additionalPrompt,
			CreatedAt: time.Now().Format(time.RFC3339),
		})
	}

	inputJSON, err := json.Marshal(inputMessages)
	if err != nil {
		return compactionOutcome{}, fmt.Errorf("marshal compaction script input: %w", err)
	}

	shell, shellArgsPrefix := shellpath.DetectShell()
	timeoutCtx, cancel := context.WithTimeout(ctx, compactionScriptTimeout)
	defer cancel()

	cmd := exec.CommandContext(timeoutCtx, shell, append(shellArgsPrefix, script)...)
	if r.workingDir != "" {
		cmd.Dir = r.workingDir
	} else if sess.WorkingDir != "" {
		cmd.Dir = sess.WorkingDir
	}
	if len(r.env) > 0 {
		cmd.Env = r.env
	}
	cmd.Stdin = bytes.NewReader(inputJSON)

	var stdout bytes.Buffer
	var stderr bytes.Buffer
	cmd.Stdout = &stdout
	cmd.Stderr = &stderr

	if err := cmd.Run(); err != nil {
		return compactionOutcome{}, fmt.Errorf("compaction script failed: %w: %s", err, strings.TrimSpace(stderr.String()))
	}

	output := strings.TrimSpace(stdout.String())
	if output == "" || !strings.HasPrefix(output, "[") {
		return compactionOutcome{}, errors.New("compaction script must output a JSON array of messages")
	}

	var compacted []chat.Message
	if err := json.Unmarshal([]byte(output), &compacted); err != nil {
		return compactionOutcome{}, fmt.Errorf("parse compaction script output: %w", err)
	}

	normalized, err := normalizeCompactionMessages(compacted)
	if err != nil {
		return compactionOutcome{}, err
	}

	return compactionOutcome{
		summary:     fmt.Sprintf("Compaction script replaced prior history with %d message(s).", len(normalized)),
		messages:    normalized,
		inputTokens: estimateMessagesTokens(normalized),
	}, nil
}

func (r *LocalRuntime) runCompaction(ctx context.Context, sess *session.Session, a *agent.Agent, messages []chat.Message, additionalPrompt string, events chan Event) (compactionOutcome, error) {
	cfg := a.CompactionConfig()
	compactionType := latest.CompactionTypeSummary
	if cfg != nil {
		compactionType = cfg.EffectiveType()
	}

	switch compactionType {
	case latest.CompactionTypeCustom:
		script := ""
		if cfg != nil {
			script = cfg.Script
		}
		if strings.TrimSpace(script) != "" {
			result, err := r.runCompactionScript(ctx, sess, script, messages, additionalPrompt)
			if err == nil {
				return result, nil
			}
			slog.Warn("Compaction script failed, falling back to built-in compaction", "agent", a.Name(), "error", err)
			if events != nil {
				events <- Warning("Compaction script failed; falling back to built-in compaction.", a.Name())
			}
		}
		// Fall through to summary-based compaction

	case latest.CompactionTypeRolling:
		return r.runRollingCompaction(messages, cfg)

	case latest.CompactionTypeAgent:
		if cfg != nil && cfg.Agent != "" {
			result, err := r.runAgentCompaction(ctx, sess, cfg.Agent, messages, additionalPrompt)
			if err == nil {
				return result, nil
			}
			slog.Warn("Agent compaction failed, falling back to built-in compaction", "agent", a.Name(), "compaction_agent", cfg.Agent, "error", err)
			if events != nil {
				events <- Warning("Agent compaction failed; falling back to built-in compaction.", a.Name())
			}
		}
		// Fall through to summary-based compaction

	case latest.CompactionTypeSummary:
		// use summary below
	}

	// Summary-based compaction (default)
	model := a.Model()
	// TODO: resolve cfg.Model override via the team's model store.
	prepared := compaction.BuildPrompt(messages, additionalPrompt)
	result, err := runSummarization(ctx, model, prepared)
	if err != nil {
		return compactionOutcome{}, err
	}

	return compactionOutcome{
		summary:     result.Summary,
		inputTokens: result.InputTokens,
		cost:        result.Cost,
	}, nil
}

// runRollingCompaction keeps only the most recent messages, discarding older ones.
func (r *LocalRuntime) runRollingCompaction(messages []chat.Message, cfg *latest.CompactionConfig) (compactionOutcome, error) {
	if cfg == nil {
		return compactionOutcome{}, errors.New("rolling compaction requires configuration")
	}

	// Separate conversation messages from system messages
	var conversationMessages []chat.Message
	for _, msg := range messages {
		if msg.Role != chat.MessageRoleSystem {
			conversationMessages = append(conversationMessages, msg)
		}
	}

	keepCount := cfg.Threshold.Messages()
	if keepCount <= 0 {
		// For token/percentage thresholds on rolling, keep half the messages
		keepCount = len(conversationMessages) / 2
	}

	if keepCount >= len(conversationMessages) {
		// Nothing to trim
		return compactionOutcome{}, nil
	}

	kept := conversationMessages[len(conversationMessages)-keepCount:]

	return compactionOutcome{
		summary:     fmt.Sprintf("Rolling compaction kept %d most recent message(s) out of %d.", len(kept), len(conversationMessages)),
		messages:    kept,
		inputTokens: estimateMessagesTokens(kept),
	}, nil
}

// runAgentCompaction delegates compaction to a named agent from the team.
func (r *LocalRuntime) runAgentCompaction(ctx context.Context, _ *session.Session, agentName string, messages []chat.Message, additionalPrompt string) (compactionOutcome, error) {
	compactionAgent, err := r.team.Agent(agentName)
	if err != nil {
		return compactionOutcome{}, fmt.Errorf("compaction agent %q not found in team: %w", agentName, err)
	}

	prepared := compaction.BuildPrompt(messages, additionalPrompt)
	result, err := runSummarization(ctx, compactionAgent.Model(), prepared)
	if err != nil {
		return compactionOutcome{}, fmt.Errorf("agent compaction failed: %w", err)
	}

	return compactionOutcome{
		summary:     result.Summary,
		inputTokens: result.InputTokens,
		cost:        result.Cost,
	}, nil
}

// doCompact runs compaction on a session and applies the result (events,
// persistence, token count updates). The agent is used to extract the
// conversation from the session and to obtain the model for summarization.
func (r *LocalRuntime) doCompact(ctx context.Context, sess *session.Session, a *agent.Agent, additionalPrompt string, events chan Event) {
	slog.Debug("Generating compacted session state", "session_id", sess.ID)

	events <- SessionCompaction(sess.ID, "started", a.Name())
	defer func() {
		events <- SessionCompaction(sess.ID, "completed", a.Name())
	}()

	messages := sess.GetMessages(a)
	if !compaction.HasConversationMessages(messages) {
		if additionalPrompt == "" {
			events <- Warning("Session is empty. Start a conversation before compacting.", a.Name())
		}
		return
	}

	result, err := r.runCompaction(ctx, sess, a, messages, additionalPrompt, events)
	if err != nil {
		slog.Error("Failed to compact session", "error", err)
		events <- Error(err.Error())
		return
	}
	if result.summary == "" && result.messages == nil {
		return
	}

	if result.messages != nil {
		sess.Messages = append(sess.Messages, session.NewCompactionItem(result.messages, result.cost))
	} else {
		sess.Messages = append(sess.Messages, session.Item{Summary: result.summary, Cost: result.cost})
	}
	sess.InputTokens = result.inputTokens
	sess.OutputTokens = 0

	slog.Debug("Compacted session", "session_id", sess.ID, "summary_length", len(result.summary), "compaction_cost", result.cost)
	if result.messages != nil {
		events <- SessionSummaryWithMessages(sess.ID, result.summary, a.Name(), result.messages)
		return
	}
	events <- SessionSummary(sess.ID, result.summary, a.Name())
}

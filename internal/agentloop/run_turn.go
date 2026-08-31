package agentloop

import (
	"context"
	"encoding/json"
	"errors"
	"math"
	"strings"
	"time"

	"grok_switch/internal/llm"
)

const (
	defaultMaxSteps  = 100
	defaultMaxRetry  = 3
	baseRetryBackoff = 750 * time.Millisecond
	maxRetryBackoff  = 30 * time.Second
)

// RunTurn 运行一个完整 turn：循环执行「模型步 → 工具执行」直到非 tool_use
// 终止、abort、或步数耗尽（对应 Kimi run-turn.ts 的收敛职责）。
func RunTurn(ctx context.Context, in RunTurnInput) (TurnResult, error) {
	maxSteps := in.MaxSteps
	if maxSteps <= 0 {
		maxSteps = defaultMaxSteps
	}
	maxRetry := in.MaxRetries
	if maxRetry <= 0 {
		maxRetry = defaultMaxRetry
	}

	var usage llm.TokenUsage
	steps := 0
	stopReason := TurnEnd
	var turnErr error

	emit := func(ev Event) {
		if in.Events != nil {
			ev.TurnID = in.TurnID
			in.Events.Dispatch(ev)
		}
	}

	emit(Event{Type: EventTurnBegin})

	// 主循环：abort 检查在循环边界。
	for {
		if err := ctx.Err(); err != nil {
			reason := TurnAborted
			if errors.Is(err, context.Canceled) {
				reason = TurnUserAbort
			}
			stopReason = reason
			emit(Event{Type: EventTurnInterrupt, StopReason: reason, Error: err.Error(), Step: steps})
			return TurnResult{StopReason: reason, Steps: steps, Usage: usage}, nil
		}
		if steps >= maxSteps {
			stopReason = TurnMaxSteps
			turnErr = &MaxStepsExceededError{Max: maxSteps}
			emit(Event{Type: EventTurnInterrupt, StopReason: stopReason, Error: turnErr.Error(), Step: steps})
			return TurnResult{StopReason: stopReason, Steps: steps, Usage: usage, Error: turnErr}, turnErr
		}
		if in.Hooks.BeforeStep != nil {
			if err := in.Hooks.BeforeStep(ctx, steps+1); err != nil {
				stopReason = TurnError
				turnErr = err
				emit(Event{Type: EventTurnInterrupt, StopReason: stopReason, Error: err.Error(), Step: steps})
				return TurnResult{StopReason: stopReason, Steps: steps, Usage: usage, Error: err}, err
			}
		}

		steps++
		stepRes, err := runModelStep(ctx, in, steps, maxRetry)
		// usage 在 LLM 返回后立即累计（无论工具是否执行，abort 也报告）。
		usage = usage.Add(stepRes.usage)
		if stepRes.usage.GrandTotal() > 0 {
			u := stepRes.usage
			acc := stepRes.accuracy
			emit(Event{Type: EventUsage, Step: steps, Usage: &u, UsageAccuracy: string(acc)})
		}
		if err != nil {
			if isUserAbort(ctx, err) {
				stopReason = TurnUserAbort
				emit(Event{Type: EventTurnInterrupt, StopReason: stopReason, Step: steps})
				return TurnResult{StopReason: stopReason, Steps: steps, Usage: usage}, nil
			}
			stopReason = TurnError
			turnErr = err
			emit(Event{Type: EventTurnInterrupt, StopReason: stopReason, Error: err.Error(), Step: steps})
			return TurnResult{StopReason: stopReason, Steps: steps, Usage: usage, Error: err}, err
		}

		if stepRes.stopReason == StopToolUse {
			// 先派发 tool_call 事件（保持 call→result 配对契约），再执行批次。
			for _, call := range stepRes.message.ToolCalls {
				args := json.RawMessage(call.Arguments)
				emit(Event{Type: EventToolCall, Step: steps, ToolCall: &ToolCallEvent{ID: call.ID, Name: call.Name, Arguments: args}})
			}
			// 执行工具批次；结果追加进历史后进入下一步。
			if in.Tools != nil {
				execToolBatch(ctx, in, stepRes.message.ToolCalls, emit, steps)
			} else {
				// 无工具执行器：把调用结果标为错误回给模型，避免死循环。
				for _, call := range stepRes.message.ToolCalls {
					appendToolResult(in.Memory, call, ToolResult{Output: "模型请求了工具调用，但当前会话没有可用工具执行器。", IsError: true})
					emit(Event{Type: EventToolResult, Step: steps, ToolResult: &ToolResultEvent{ID: call.ID, Name: call.Name, Output: "no executor", IsError: true}})
				}
			}
			continue
		}

		stopReason = turnStopFromStep(stepRes.stopReason)
		break
	}

	emit(Event{Type: EventTurnEnd, StopReason: stopReason, Step: steps, Usage: &usage})

	// 宿主可选续跑（goal mode 切入点；一期不启用）。
	if in.Hooks.ShouldContinueAfterStop != nil && in.Hooks.ShouldContinueAfterStop(ctx, ContinueCheck{
		Step:       steps,
		StopReason: stepStopFromTurn(stopReason),
		Usage:      usage,
	}) {
		// 一期：递归续跑一次语义过于激进，返回由宿主自行再次调用 RunTurn。
		return TurnResult{StopReason: stopReason, Steps: steps, Usage: usage}, nil
	}
	return TurnResult{StopReason: stopReason, Steps: steps, Usage: usage}, turnErr
}

// stepOutput 是单个模型步的结果。
type stepOutput struct {
	message    llm.Message
	stopReason StepStopReason
	usage      llm.TokenUsage
	accuracy   llm.UsageAccuracy
}

// runModelStep 执行一次模型调用（含重试退避），并把 assistant 消息写入历史。
func runModelStep(ctx context.Context, in RunTurnInput, step, maxRetry int) (stepOutput, error) {
	emit := func(ev Event) {
		if in.Events != nil {
			ev.TurnID = in.TurnID
			ev.Step = step
			in.Events.Dispatch(ev)
		}
	}
	emit(Event{Type: EventStepBegin})

	opts := llm.GenerateOptions{Effort: in.Effort}
	if in.Events != nil {
		opts.OnDelta = func(part llm.StreamedPart) {
			switch v := part.(type) {
			case llm.TextDelta:
				emit(Event{Type: EventTextDelta, Text: v.Text})
			case llm.ThinkDelta:
				emit(Event{Type: EventThinkDelta, Think: v.Text})
			}
		}
	}

	var lastErr error
	for attempt := 0; attempt <= maxRetry; attempt++ {
		if err := ctx.Err(); err != nil {
			return stepOutput{}, err
		}
		history := in.Memory.History()
		res, err := in.Provider.Generate(ctx, in.SystemPrompt, toolSchemas(in.Tools), history, opts)
		if err == nil {
			in.Memory.Append(res.Message)
			emit(Event{Type: EventStepEnd, StopReason: TurnStopReason(stepStopFromLLM(res.FinishReason))})
			return stepOutput{
				message:    res.Message,
				stopReason: stepStopFromLLM(res.FinishReason),
				usage:      res.Usage,
				accuracy:   res.Accuracy,
			}, nil
		}
		lastErr = err
		if !shouldRetry(err) || attempt == maxRetry {
			break
		}
		// 指数退避；ctx 取消立即返回。
		if !sleepCtx(ctx, retryDelay(attempt, err)) {
			return stepOutput{}, ctx.Err()
		}
	}
	return stepOutput{}, lastErr
}

// toolSchemas 获取当前工具定义。
func toolSchemas(t ToolExecutor) []llm.Tool {
	if t == nil {
		return nil
	}
	return t.Schemas()
}

// execToolBatch 顺序执行一个工具调用批次（P2 从简；只读并行是二期优化）。
// 每个 call 必有配对 result 事件。
func execToolBatch(ctx context.Context, in RunTurnInput, calls []llm.ToolCall, emit func(Event), step int) {
	for _, call := range calls {
		if err := ctx.Err(); err != nil {
			// turn 已被取消：给剩余调用补终止结果，保持配对契约。
			appendToolResult(in.Memory, call, ToolResult{Output: "用户取消了本次任务。", IsError: true})
			emit(Event{Type: EventToolResult, Step: step, ToolResult: &ToolResultEvent{ID: call.ID, Name: call.Name, IsError: true}})
			continue
		}
		// 权限闸。
		decision := DecAllow
		var denyReason string
		if in.PermGate != nil {
			switch in.PermGate.Check(call) {
			case DecAllow:
				decision = DecAllow
			case DecDeny:
				decision = DecDeny
				denyReason = "权限规则拒绝了该操作。"
			default:
				permRes := in.PermGate.WaitForDecision(ctx, call)
				decision = permRes.Decision
				denyReason = permRes.Reason
				if denyReason == "" {
					denyReason = "用户拒绝了该操作。"
				}
			}
		}

		var result ToolResult
		switch decision {
		case DecDeny:
			result = ToolResult{Output: denyReason, IsError: true}
		default:
			res, err := in.Tools.Execute(ctx, call)
			if err != nil {
				result = ToolResult{Output: "工具执行失败: " + err.Error(), IsError: true}
			} else {
				result = res
			}
		}
		appendToolResult(in.Memory, call, result)
		emit(Event{
			Type: EventToolResult,
			Step: step,
			ToolResult: &ToolResultEvent{
				ID: call.ID, Name: call.Name,
				Output: result.Output, IsError: result.IsError, Truncated: result.Truncated,
			},
		})
	}
}

// appendToolResult 把工具结果写进历史。
func appendToolResult(mem Memory, call llm.ToolCall, result ToolResult) {
	parts := []llm.ContentPart{llm.TextPart{Text: result.ResultJSON()}}
	mem.Append(llm.Message{Role: llm.RoleTool, ToolCallID: call.ID, Parts: parts})
}

// shouldRetry 判断错误是否值得重试。
func shouldRetry(err error) bool {
	apiErr, ok := err.(*llm.APIError)
	if !ok {
		return false
	}
	return llm.RetryableKind(apiErr.Kind)
}

// retryDelay 计算指数退避时长；限流尊重 Retry-After。
func retryDelay(attempt int, err error) time.Duration {
	if apiErr, ok := err.(*llm.APIError); ok && apiErr.RetryAfter > 0 {
		return apiErr.RetryAfter
	}
	d := time.Duration(float64(baseRetryBackoff) * math.Pow(2, float64(attempt)))
	if d > maxRetryBackoff {
		d = maxRetryBackoff
	}
	return d
}

// sleepCtx 可取消的 sleep；返回 false 表示 ctx 已结束。
func sleepCtx(ctx context.Context, d time.Duration) bool {
	if d <= 0 {
		return ctx.Err() == nil
	}
	t := time.NewTimer(d)
	defer t.Stop()
	select {
	case <-ctx.Done():
		return false
	case <-t.C:
		return true
	}
}

// isUserAbort 判断错误链是否为用户取消。
func isUserAbort(ctx context.Context, err error) bool {
	if errors.Is(err, context.Canceled) {
		return true
	}
	if ctx.Err() != nil && errors.Is(ctx.Err(), context.Canceled) {
		return true
	}
	return false
}

func turnStopFromStep(r StepStopReason) TurnStopReason {
	switch r {
	case StopMaxToken:
		return TurnMaxToken
	case StopFiltered:
		return TurnFiltered
	default:
		return TurnEnd
	}
}

func stepStopFromTurn(r TurnStopReason) StepStopReason {
	switch r {
	case TurnMaxToken:
		return StopMaxToken
	case TurnFiltered:
		return StopFiltered
	default:
		return StopEndTurn
	}
}

func stepStopFromLLM(raw string) StepStopReason {
	switch llm.NormalizeFinishReason(raw) {
	case "tool_use":
		return StopToolUse
	case "max_tokens":
		return StopMaxToken
	case "filtered":
		return StopFiltered
	case "stop":
		return StopEndTurn
	default:
		return StopUnknown
	}
}

// compactJSON 截断 JSON 文本用于日志。
func compactJSON(s string) string {
	s = strings.TrimSpace(s)
	if len(s) > 200 {
		return s[:200] + "…"
	}
	return s
}

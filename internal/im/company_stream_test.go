package im

import (
	"context"
	"errors"
	"strings"
	"sync"
	"testing"
	"testing/synctest"
	"time"
	"unicode/utf8"

	"github.com/Tencent/WeKnora/internal/companyidentity"
	"github.com/Tencent/WeKnora/internal/event"
	"github.com/Tencent/WeKnora/internal/types"
	"github.com/stretchr/testify/require"
)

type companyStreamAdapter struct {
	*fullOutputAdapter
	mu         sync.Mutex
	chunks     []string
	frames     []string
	cache      string
	files      int
	fileBytes  [][]byte
	onFinalize func(int)
	finalizes  int
	updateErr  error
	endFrames  []string
}

func (a *companyStreamAdapter) UpdateStreamContent(ctx context.Context, msg *IncomingMessage, id, content string) error {
	a.mu.Lock()
	defer a.mu.Unlock()
	a.chunks = append(a.chunks, content)
	a.frames = append(a.frames, content)
	a.cache = content
	if err := a.fullOutputAdapter.UpdateStreamContent(ctx, msg, id, content); err != nil {
		return err
	}
	return a.updateErr
}

func (a *companyStreamAdapter) FinalizeStream(ctx context.Context, msg *IncomingMessage, id, content string) error {
	a.mu.Lock()
	defer a.mu.Unlock()
	a.frames = append(a.frames, content)
	a.cache = content
	a.finalizes++
	if a.onFinalize != nil {
		a.onFinalize(a.finalizes)
	}
	return a.fullOutputAdapter.FinalizeStream(ctx, msg, id, content)
}

func (a *companyStreamAdapter) EndStream(ctx context.Context, msg *IncomingMessage, id string) error {
	a.mu.Lock()
	defer a.mu.Unlock()
	a.frames = append(a.frames, a.cache)
	a.endFrames = append(a.endFrames, a.cache)
	return a.fullOutputAdapter.EndStream(ctx, msg, id)
}

func (a *companyStreamAdapter) MaxFileSize() int64 { return 1024 }
func (a *companyStreamAdapter) SendFile(_ context.Context, _ *IncomingMessage, _ string, data []byte) error {
	a.mu.Lock()
	defer a.mu.Unlock()
	a.files++
	a.fileBytes = append(a.fileBytes, append([]byte(nil), data...))
	return nil
}

func (a *companyStreamAdapter) SendReply(ctx context.Context, msg *IncomingMessage, reply *ReplyMessage) error {
	a.mu.Lock()
	defer a.mu.Unlock()
	a.frames = append(a.frames, reply.Content)
	return a.fullOutputAdapter.SendReply(ctx, msg, reply)
}

func (a *companyStreamAdapter) progressSnapshot() ([]string, string) {
	a.mu.Lock()
	defer a.mu.Unlock()
	return append([]string(nil), a.chunks...), a.finalContent
}

type companyStreamChecker struct {
	*testEmployeeChecker
	mu sync.Mutex
}

func (c *companyStreamChecker) LookupWeCom(ctx context.Context, id string) (*companyidentity.Member, error) {
	c.mu.Lock()
	defer c.mu.Unlock()
	return c.testEmployeeChecker.LookupWeCom(ctx, id)
}

func (c *companyStreamChecker) Check(ctx context.Context, binding companyidentity.Binding) (*companyidentity.Member, error) {
	c.mu.Lock()
	defer c.mu.Unlock()
	return c.testEmployeeChecker.Check(ctx, binding)
}

func (c *companyStreamChecker) revoke() {
	c.mu.Lock()
	defer c.mu.Unlock()
	c.allowed = false
}

func (c *companyStreamChecker) checkCount() int {
	c.mu.Lock()
	defer c.mu.Unlock()
	return c.checks
}

func newCompanyStreamHarness(t *testing.T, run func(context.Context, *event.EventBus) error) (*Service, *companyStreamAdapter, *companyStreamChecker, context.Context, *IncomingMessage) {
	t.Helper()
	svc, base, _, _ := newFullOutputHarness("")
	svc.sessionService = &streamExitSessionService{run: run}
	svc.messageService = &deliveryMessageService{}
	checker := &companyStreamChecker{testEmployeeChecker: employeeFixture()}
	svc.employeeChecker = checker
	svc.companyDownloadBase = "https://identity.example/company/knowledge"
	msg := &IncomingMessage{Platform: PlatformWeCom, UserID: "member", Content: "比较芯片 RAM"}
	ctx, err := svc.authorizeIMIdentity(context.Background(), &IMChannel{ID: "channel", TenantID: 7, Platform: "wecom", OutputMode: "stream"}, msg)
	require.NoError(t, err)
	return svc, &companyStreamAdapter{fullOutputAdapter: base}, checker, ctx, msg
}

func runCompanyStream(svc *Service, adapter *companyStreamAdapter, ctx context.Context, msg *IncomingMessage, agent *types.CustomAgent) error {
	return svc.handleMessageStream(ctx, msg, &types.Session{ID: "session"}, agent, nil, nil, nil, adapter, adapter, "member-key", nil)
}

func emitCompanyAnswer(ctx context.Context, bus *event.EventBus, text string, done bool) {
	_ = bus.Emit(ctx, event.Event{ID: "answer", Type: event.EventAgentFinalAnswer, Data: event.AgentFinalAnswerData{Content: text, Done: done}})
}

func TestCompanyStreamShowsImmediateStagesAndIncrementalPublicAnswer(t *testing.T) {
	for _, agentMode := range []bool{false, true} {
		t.Run(map[bool]string{false: "quick QA", true: "agent"}[agentMode], func(t *testing.T) {
			synctest.Test(t, func(t *testing.T) {
				var adapter *companyStreamAdapter
				svc, sender, _, ctx, msg := newCompanyStreamHarness(t, func(ctx context.Context, bus *event.EventBus) error {
					chunks, _ := adapter.progressSnapshot()
					require.Equal(t, []string{"已收到，正在处理…"}, chunks, "visible acknowledgment must precede QA")
					_ = bus.Emit(ctx, event.Event{Type: event.EventAgentThought, Data: event.AgentThoughtData{Content: "SECRET_THOUGHT /private/path"}})
					_ = bus.Emit(ctx, event.Event{Type: event.EventAgentToolCall, Data: event.AgentToolCallData{ToolName: "search_knowledge", Arguments: map[string]any{"query": "SECRET_ARGUMENT"}, Hint: "SECRET_HINT"}})
					time.Sleep(2 * time.Second)
					_ = bus.Emit(ctx, event.Event{Type: event.EventAgentToolResult, Data: event.AgentToolResultData{ToolName: "search_knowledge", Output: "SECRET_OUTPUT", Error: "SECRET_TOOL_ERROR", Data: map[string]any{"file": "/private/path"}}})
					emitCompanyAnswer(ctx, bus, "第一部分", false)
					time.Sleep(2 * time.Second)
					emitCompanyAnswer(ctx, bus, "，第二部分", false)
					time.Sleep(2 * time.Second)
					emitCompanyAnswer(ctx, bus, "", true)
					if agentMode {
						_ = bus.Emit(ctx, event.Event{Type: event.EventAgentComplete, Data: event.AgentCompleteData{FinalAnswer: "第一部分，第二部分"}})
					}
					return nil
				})
				adapter = sender
				var agent *types.CustomAgent
				if agentMode {
					agent = streamExitAgent()
				}
				done := make(chan error, 1)
				go func() { done <- runCompanyStream(svc, adapter, ctx, msg, agent) }()
				synctest.Wait()
				chunks, _ := adapter.progressSnapshot()
				require.Equal(t, []string{"已收到，正在处理…"}, chunks)
				time.Sleep(1500 * time.Millisecond)
				synctest.Wait()
				chunks, _ = adapter.progressSnapshot()
				require.Contains(t, chunks, "正在检索资料…")
				time.Sleep(2 * time.Second)
				synctest.Wait()
				chunks, final := adapter.progressSnapshot()
				require.Contains(t, chunks[len(chunks)-1], "第一部分")
				require.Empty(t, final, "first public answer must arrive before terminal event")
				time.Sleep(2 * time.Second)
				synctest.Wait()
				chunks, final = adapter.progressSnapshot()
				require.Contains(t, chunks[len(chunks)-1], "第一部分，第二部分")
				require.Empty(t, final)
				require.NoError(t, <-done)
				require.Equal(t, "第一部分，第二部分", adapter.finalContent)
				require.Equal(t, []string{"第一部分，第二部分"}, adapter.endFrames)
				require.Zero(t, adapter.files)
				for _, frame := range adapter.frames {
					require.NotContains(t, frame, "SECRET")
					require.NotContains(t, frame, "/private/path")
					require.NotContains(t, frame, "思考过程")
				}
				require.Equal(t, "第一部分，第二部分", svc.messageService.(*deliveryMessageService).saved.Content)
			})
		})
	}
}

func TestCompanyStreamHeartbeatDoesNotPollOrResendUnchangedContent(t *testing.T) {
	synctest.Test(t, func(t *testing.T) {
		release := make(chan struct{})
		svc, adapter, checker, ctx, msg := newCompanyStreamHarness(t, func(ctx context.Context, bus *event.EventBus) error {
			<-release
			emitCompanyAnswer(ctx, bus, "完成", true)
			return nil
		})
		done := make(chan error, 1)
		go func() { done <- runCompanyStream(svc, adapter, ctx, msg, nil) }()
		synctest.Wait()
		time.Sleep(9 * time.Second)
		synctest.Wait()
		chunks, _ := adapter.progressSnapshot()
		require.Len(t, chunks, 1)
		require.Equal(t, 2, checker.checkCount(), "no directory poll for unchanged progress")
		time.Sleep(2 * time.Second)
		synctest.Wait()
		chunks, _ = adapter.progressSnapshot()
		require.Len(t, chunks, 2)
		require.Contains(t, chunks[1], "已用时 10 秒")
		require.Equal(t, 3, checker.checkCount())
		close(release)
		require.NoError(t, <-done)
	})
}

func TestCompanyStreamRetractsToolPreambleAndReplaceDraft(t *testing.T) {
	synctest.Test(t, func(t *testing.T) {
		svc, adapter, _, ctx, msg := newCompanyStreamHarness(t, func(ctx context.Context, bus *event.EventBus) error {
			emitCompanyAnswer(ctx, bus, "公开前言", false)
			time.Sleep(2 * time.Second)
			_ = bus.Emit(ctx, event.Event{Type: event.EventAgentToolCall, Data: event.AgentToolCallData{ToolName: "read_document", ToolCallID: "tool"}})
			time.Sleep(2 * time.Second)
			emitCompanyAnswer(ctx, bus, "失败草稿", false)
			time.Sleep(2 * time.Second)
			_ = bus.Emit(ctx, event.Event{ID: "answer", Type: event.EventAgentFinalAnswer, Data: event.AgentFinalAnswerData{Replace: true}})
			emitCompanyAnswer(ctx, bus, "正确答案", false)
			time.Sleep(2 * time.Second)
			emitCompanyAnswer(ctx, bus, "", true)
			return bus.Emit(ctx, event.Event{Type: event.EventAgentComplete, Data: event.AgentCompleteData{FinalAnswer: "正确答案"}})
		})
		done := make(chan error, 1)
		go func() { done <- runCompanyStream(svc, adapter, ctx, msg, streamExitAgent()) }()
		require.NoError(t, <-done)
		require.Equal(t, "正确答案", adapter.finalContent)
		require.Equal(t, "正确答案", svc.messageService.(*deliveryMessageService).saved.Content)
		require.Contains(t, adapter.chunks, "正在核对原文…")
		for _, frame := range adapter.chunks {
			if strings.Contains(frame, "正确答案") || strings.Contains(frame, "核对原文") {
				require.NotContains(t, frame, "公开前言")
				require.NotContains(t, frame, "失败草稿")
			}
		}
	})
}

func TestCompanyStreamFailureAndCancellationAreTerminalAndGeneric(t *testing.T) {
	for _, scenario := range []string{"error event", "post complete error", "cancel", "timeout"} {
		t.Run(scenario, func(t *testing.T) {
			synctest.Test(t, func(t *testing.T) {
				svc, adapter, _, ctx, msg := newCompanyStreamHarness(t, func(ctx context.Context, bus *event.EventBus) error {
					emitCompanyAnswer(ctx, bus, "未完成的部分答案", false)
					time.Sleep(2 * time.Second)
					if scenario == "error event" {
						return bus.Emit(ctx, event.Event{Type: event.EventError, Data: event.ErrorData{Error: "SECRET_BACKEND_ERROR /private/path"}})
					}
					if scenario == "post complete error" {
						_ = bus.Emit(ctx, event.Event{Type: event.EventAgentComplete, Data: event.AgentCompleteData{FinalAnswer: "未完成的部分答案"}})
						return errors.New("SECRET_BACKEND_ERROR /private/path")
					}
					<-ctx.Done()
					return ctx.Err()
				})
				var cancel context.CancelFunc
				ctx, cancel = context.WithCancel(ctx)
				defer cancel()
				msg.Content = "请发我 datasheet 原件"
				done := make(chan error, 1)
				go func() { done <- runCompanyStream(svc, adapter, ctx, msg, streamExitAgent()) }()
				if scenario == "cancel" {
					time.Sleep(3 * time.Second)
					cancel()
				}
				require.NoError(t, <-done)
				want := imErrorFallback
				if scenario == "cancel" {
					want = imCancelledFallback
				}
				if scenario == "timeout" {
					want = companyStreamTimedOut
				}
				require.Equal(t, want, adapter.finalContent)
				require.Equal(t, []string{want}, adapter.endFrames)
				require.Zero(t, adapter.files)
				require.NotContains(t, adapter.finalContent, "SECRET")
				require.Equal(t, want, svc.messageService.(*deliveryMessageService).saved.Content)
			})
		})
	}
}

func TestCompanyStreamRechecksEveryChangedPartialAndStopsAfterRevocation(t *testing.T) {
	synctest.Test(t, func(t *testing.T) {
		var checker *companyStreamChecker
		svc, adapter, check, ctx, msg := newCompanyStreamHarness(t, func(ctx context.Context, bus *event.EventBus) error {
			emitCompanyAnswer(ctx, bus, "先前允许的回答", false)
			time.Sleep(2 * time.Second)
			checker.revoke()
			emitCompanyAnswer(ctx, bus, "撤权后绝不能发送", false)
			<-ctx.Done()
			return ctx.Err()
		})
		checker = check
		require.Error(t, runCompanyStream(svc, adapter, ctx, msg, streamExitAgent()))
		require.GreaterOrEqual(t, checker.checkCount(), 5)
		require.Contains(t, strings.Join(adapter.chunks, "\n"), "先前允许的回答")
		for _, frame := range adapter.frames {
			require.NotContains(t, frame, "撤权后绝不能发送")
		}
		require.Equal(t, []string{companyStreamDenied}, adapter.endFrames)
		require.Equal(t, companyStreamDenied, svc.messageService.(*deliveryMessageService).saved.Content)
	})
}

func TestCompanyStreamRechecksFinalizeEndAndPlainFallback(t *testing.T) {
	for _, boundary := range []string{"final", "end", "plain fallback"} {
		t.Run(boundary, func(t *testing.T) {
			svc, adapter, checker, ctx, msg := newCompanyStreamHarness(t, func(ctx context.Context, bus *event.EventBus) error {
				emitCompanyAnswer(ctx, bus, "受保护正文", true)
				return nil
			})
			if boundary == "final" {
				svc.sessionService = &streamExitSessionService{run: func(ctx context.Context, bus *event.EventBus) error {
					emitCompanyAnswer(ctx, bus, "受保护正文", true)
					checker.revoke()
					return nil
				}}
			} else {
				adapter.onFinalize = func(int) { checker.revoke() }
				if boundary == "plain fallback" {
					adapter.finalizeErr = errors.New("transport failed")
				}
			}
			require.Error(t, runCompanyStream(svc, adapter, ctx, msg, streamExitAgent()))
			for _, frame := range adapter.endFrames {
				require.Equal(t, companyStreamDenied, frame)
			}
			if adapter.plainReplies > 0 {
				require.Equal(t, companyStreamDenied, adapter.plainContent)
			}
			require.Zero(t, adapter.files)
		})
	}
}

type failingCompanyChecker struct{ *companyStreamChecker }

func (c *failingCompanyChecker) LookupWeCom(context.Context, string) (*companyidentity.Member, error) {
	return nil, errors.New("directory down SECRET")
}

func TestCompanyStreamDirectoryFailureClosesWithoutCachedAnswer(t *testing.T) {
	svc, adapter, checker, ctx, msg := newCompanyStreamHarness(t, func(context.Context, *event.EventBus) error { return nil })
	svc.employeeChecker = &failingCompanyChecker{checker}
	require.Error(t, runCompanyStream(svc, adapter, ctx, msg, nil))
	require.Equal(t, companyStreamDenied, adapter.finalContent)
	require.Equal(t, []string{companyStreamDenied}, adapter.endFrames)
	require.Empty(t, adapter.chunks)
}

func TestCompanyStreamHoldsPartialLinksAndInlineThinkingTags(t *testing.T) {
	synctest.Test(t, func(t *testing.T) {
		raw := "[手册](/platform/source-download/" + originalTestID + ")"
		svc, adapter, _, ctx, msg := newCompanyStreamHarness(t, func(ctx context.Context, bus *event.EventBus) error {
			emitCompanyAnswer(ctx, bus, "<thi", false)
			time.Sleep(2 * time.Second)
			emitCompanyAnswer(ctx, bus, "nk>SECRET_INLINE</think>答案 "+raw[:20], false)
			time.Sleep(2 * time.Second)
			emitCompanyAnswer(ctx, bus, raw[20:], false)
			time.Sleep(2 * time.Second)
			emitCompanyAnswer(ctx, bus, "", true)
			return nil
		})
		require.NoError(t, runCompanyStream(svc, adapter, ctx, msg, streamExitAgent()))
		for _, frame := range adapter.frames {
			require.NotContains(t, frame, "SECRET_INLINE")
			require.NotContains(t, frame, "<thi")
			require.NotContains(t, frame, "/platform/source-download/")
		}
		require.Contains(t, adapter.finalContent, "https://identity.example/company/knowledge/"+originalTestID+"/download")
		require.Zero(t, adapter.files)
	})
}

func TestCompanyStreamOriginalFilesOnlyAfterSuccessfulExplicitRequest(t *testing.T) {
	for _, query := range []string{"AB5682B和AB5682C有什么区别", "请发我 datasheet 原件"} {
		t.Run(query, func(t *testing.T) {
			svc, adapter, _, ctx, msg := newCompanyStreamHarness(t, func(ctx context.Context, bus *event.EventBus) error {
				emitCompanyAnswer(ctx, bus, "[original](/platform/source-download/"+originalTestID+")", true)
				return nil
			})
			msg.Content = query
			svc.knowledgeService = &originalTestService{row: &types.Knowledge{ID: originalTestID, TenantID: 7, KnowledgeBaseID: "vendor", FileSize: 8}, data: []byte("%PDF-1.4")}
			agent := &types.CustomAgent{Config: types.CustomAgentConfig{KBSelectionMode: "selected", KnowledgeBases: []string{"vendor"}}}
			require.NoError(t, runCompanyStream(svc, adapter, ctx, msg, agent))
			want := 0
			if requestsOriginalFiles(query) {
				want = 1
			}
			require.Equal(t, want, adapter.files)
			if want > 0 {
				require.Equal(t, [][]byte{[]byte("%PDF-1.4")}, adapter.fileBytes)
				require.Equal(t, []string{adapter.finalContent}, adapter.endFrames, "attachment requires final stream to have ended")
			}
		})
	}
}

type companyStreamCreateFailure struct {
	deliveryMessageService
	calls  int
	failAt int
}

func (s *companyStreamCreateFailure) CreateMessage(ctx context.Context, msg *types.Message) (*types.Message, error) {
	s.calls++
	if s.calls == s.failAt {
		return nil, errors.New("SECRET_DATABASE_PATH")
	}
	return s.deliveryMessageService.CreateMessage(ctx, msg)
}

func TestCompanyStreamMessageCreateFailureEndsVisibleProgress(t *testing.T) {
	for _, at := range []int{1, 2} {
		svc, adapter, _, ctx, msg := newCompanyStreamHarness(t, func(context.Context, *event.EventBus) error {
			t.Fatal("QA should not start after message creation fails")
			return nil
		})
		svc.messageService = &companyStreamCreateFailure{failAt: at}
		require.Error(t, runCompanyStream(svc, adapter, ctx, msg, nil))
		require.Equal(t, []string{"已收到，正在处理…"}, adapter.chunks)
		require.Equal(t, imErrorFallback, adapter.finalContent)
		require.Equal(t, []string{imErrorFallback}, adapter.endFrames)
	}
}

func TestCompanyStreamTransportFallbackStillAuthorizesAndDeliversAnswer(t *testing.T) {
	for _, failure := range []string{"start", "update", "final", "end", "final and plain"} {
		t.Run(failure, func(t *testing.T) {
			svc, adapter, _, ctx, msg := newCompanyStreamHarness(t, func(ctx context.Context, bus *event.EventBus) error {
				emitCompanyAnswer(ctx, bus, "最终答案", true)
				return nil
			})
			transportErr := errors.New("SECRET_TRANSPORT_ERROR")
			switch failure {
			case "start":
				adapter.startErr = transportErr
			case "update":
				adapter.updateErr = transportErr
			case "final":
				adapter.finalizeErr = transportErr
			case "end":
				adapter.endErr = transportErr
			case "final and plain":
				adapter.finalizeErr, adapter.sendErr = transportErr, transportErr
			}
			err := runCompanyStream(svc, adapter, ctx, msg, nil)
			if failure == "final and plain" {
				require.Error(t, err)
			} else {
				require.NoError(t, err)
			}
			if failure == "start" || failure == "final" || failure == "end" {
				require.Equal(t, "最终答案", adapter.plainContent)
			} else {
				require.Equal(t, "最终答案", adapter.finalContent)
			}
			for _, frame := range adapter.frames {
				require.NotContains(t, frame, "SECRET")
			}
		})
	}
}

func TestCompanyStreamExplicitChannelModeSelectsSafeStreaming(t *testing.T) {
	synctest.Test(t, func(t *testing.T) {
		svc, adapter, _, ctx, msg := newCompanyStreamHarness(t, func(ctx context.Context, bus *event.EventBus) error {
			emitCompanyAnswer(ctx, bus, "第一段", false)
			time.Sleep(2 * time.Second)
			emitCompanyAnswer(ctx, bus, "第二段", true)
			return nil
		})
		ctx, cancel := context.WithCancel(ctx)
		svc.executeQARequest(&qaRequest{ctx: ctx, cancel: cancel, msg: msg, session: &types.Session{ID: "session"},
			adapter: adapter, channel: &IMChannel{Platform: "wecom", OutputMode: "stream"}, userKey: "member-key"})
		require.GreaterOrEqual(t, len(adapter.chunks), 2)
		require.Contains(t, strings.Join(adapter.chunks, "\n"), "第一段")
		require.Equal(t, "第一段第二段", adapter.finalContent)
	})
}

func TestCompanyStreamIgnoresRetractionOfAnOlderAnswerID(t *testing.T) {
	svc, adapter, _, ctx, msg := newCompanyStreamHarness(t, func(ctx context.Context, bus *event.EventBus) error {
		_ = bus.Emit(ctx, event.Event{ID: "old", Type: event.EventAgentFinalAnswer, Data: event.AgentFinalAnswerData{Content: "已替换草稿"}})
		_ = bus.Emit(ctx, event.Event{ID: "new", Type: event.EventAgentFinalAnswer, Data: event.AgentFinalAnswerData{Content: "当前答案"}})
		_ = bus.Emit(ctx, event.Event{ID: "old", Type: event.EventAgentFinalAnswer, Data: event.AgentFinalAnswerData{Replace: true}})
		_ = bus.Emit(ctx, event.Event{ID: "old", Type: event.EventAgentFinalAnswer, Data: event.AgentFinalAnswerData{Done: true}})
		return nil
	})
	require.NoError(t, runCompanyStream(svc, adapter, ctx, msg, streamExitAgent()))
	require.Equal(t, "当前答案", adapter.finalContent)
	require.Equal(t, "当前答案", svc.messageService.(*deliveryMessageService).saved.Content)
}

func TestCompanyStreamFinalRemovesNonceThinkingAndHiddenFileCandidates(t *testing.T) {
	for _, mode := range []string{"quick", "complete only", "start fallback"} {
		t.Run(mode, func(t *testing.T) {
			hidden := "<think:abcdef>SECRET_NONCE [hidden](/platform/source-download/" + originalTestID + ")</think:abcdef>公开答案"
			svc, adapter, _, ctx, msg := newCompanyStreamHarness(t, func(ctx context.Context, bus *event.EventBus) error {
				if mode == "complete only" {
					return bus.Emit(ctx, event.Event{Type: event.EventAgentComplete, Data: event.AgentCompleteData{FinalAnswer: hidden}})
				}
				emitCompanyAnswer(ctx, bus, hidden, true)
				return nil
			})
			svc.knowledgeService = &originalTestService{row: &types.Knowledge{ID: originalTestID, TenantID: 7, KnowledgeBaseID: "vendor", FileSize: 8}, data: []byte("%PDF-1.4")}
			msg.Content = "请发我 datasheet 原件"
			var agent *types.CustomAgent
			if mode == "complete only" {
				agent = streamExitAgent()
			}
			if mode == "start fallback" {
				adapter.startErr = errors.New("no stream")
			}
			require.NoError(t, runCompanyStream(svc, adapter, ctx, msg, agent))
			if mode == "start fallback" {
				require.Equal(t, "公开答案", adapter.plainContent)
			} else {
				require.Equal(t, "公开答案", adapter.finalContent)
			}
			require.Equal(t, "公开答案", svc.messageService.(*deliveryMessageService).saved.Content)
			require.Zero(t, adapter.files)
			require.Zero(t, svc.knowledgeService.(*originalTestService).opens)
		})
	}
}

func TestCompanyStreamBoundsEveryFrameAndHoldsTruncatedLinks(t *testing.T) {
	synctest.Test(t, func(t *testing.T) {
		long := strings.Repeat("中文内容", 2500)
		svc, adapter, _, ctx, msg := newCompanyStreamHarness(t, func(ctx context.Context, bus *event.EventBus) error {
			emitCompanyAnswer(ctx, bus, long, false)
			time.Sleep(2 * time.Second)
			emitCompanyAnswer(ctx, bus, "", true)
			return nil
		})
		require.NoError(t, runCompanyStream(svc, adapter, ctx, msg, nil))
		for _, frame := range adapter.frames {
			require.LessOrEqual(t, len(frame), companyStreamMaxBytes)
			require.True(t, utf8.ValidString(frame))
		}
		require.Contains(t, adapter.finalContent, "内容较长，已截断")
		prefix := strings.Repeat("a", companyStreamMaxBytes-300)
		clipped := companyStreamLimit(prefix + "[手册](https://identity.example/" + strings.Repeat("b", 500) + ")")
		require.NotContains(t, clipped, "[手册](")
		require.NotContains(t, clipped, "https://")
		require.LessOrEqual(t, len(clipped), companyStreamMaxBytes)
		require.Equal(t, "答案 ", companyStreamHoldback("答案 https://host/platform/source-download/12345678-"))
		require.Equal(t, "答案 ", companyStreamHoldback("答案 /platform/source-download/12345678-"))
		require.Equal(t, "说明 ]( 后续文字", companyStreamHoldback("说明 ]( 后续文字"))
	})
}

type atomicCompanyStreamAdapter struct{ *companyStreamAdapter }

func (a *atomicCompanyStreamAdapter) EndStreamWithContent(ctx context.Context, msg *IncomingMessage, id, content string) error {
	a.mu.Lock()
	a.cache = content
	a.mu.Unlock()
	return a.EndStream(ctx, msg, id)
}

func TestCompanyStreamAtomicFinishClearsFailedReplacement(t *testing.T) {
	for _, revoke := range []bool{false, true} {
		svc, base, checker, ctx, msg := newCompanyStreamHarness(t, func(ctx context.Context, bus *event.EventBus) error {
			emitCompanyAnswer(ctx, bus, "受保护最终答案", true)
			return nil
		})
		base.finalizeErr = errors.New("replace failed")
		if revoke {
			base.onFinalize = func(int) { checker.revoke() }
		}
		adapter := &atomicCompanyStreamAdapter{base}
		err := svc.handleMessageStream(ctx, msg, &types.Session{ID: "session"}, streamExitAgent(), nil, nil, nil, adapter, adapter, "member-key", nil)
		want := "受保护最终答案"
		if revoke {
			require.Error(t, err)
			want = companyStreamDenied
		} else {
			require.NoError(t, err)
		}
		require.Equal(t, []string{want}, base.endFrames)
		require.Zero(t, base.plainReplies)
	}
}

func TestCompanyStreamStartFallbackDoesNotTreatPartialErrorAsSuccessfulAnswer(t *testing.T) {
	svc, adapter, _, ctx, msg := newCompanyStreamHarness(t, func(ctx context.Context, bus *event.EventBus) error {
		emitCompanyAnswer(ctx, bus, "未完成答案 [original](/platform/source-download/"+originalTestID+")", false)
		return errors.New("SECRET_BACKEND_ERROR")
	})
	adapter.startErr = errors.New("stream unavailable")
	msg.Content = "请发我 datasheet 原件"
	svc.knowledgeService = &originalTestService{row: &types.Knowledge{ID: originalTestID, TenantID: 7, KnowledgeBaseID: "vendor", FileSize: 8}, data: []byte("%PDF-1.4")}
	require.NoError(t, runCompanyStream(svc, adapter, ctx, msg, streamExitAgent()))
	require.Equal(t, imErrorFallback, adapter.plainContent)
	require.Equal(t, imErrorFallback, svc.messageService.(*deliveryMessageService).saved.Content)
	require.Zero(t, adapter.files)
	require.Zero(t, svc.knowledgeService.(*originalTestService).opens)
}

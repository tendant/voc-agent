package agent

// Goal mode: the user states an outcome ("get me ready for Thursday's
// deploy") and voc works out the steps, asks once before acting, carries
// them out with the existing tools and guards, and reports what happened.
//
//	plan (local model with thinking, JSON schema) → approve (if it acts)
//	→ for each step: fill in arguments (JSON schema) → run → record
//	→ re-plan once on failure → report from results (claim-checked)
//
// Tasks are journaled in SQLite. A task cut off by a restart is resumable;
// steps reuse their task/step idempotency key, so finished side effects
// aren't repeated.

import (
	"context"
	"encoding/json"
	"fmt"
	"regexp"
	"strings"
	"time"

	"github.com/tendant/voc-agent/internal/llm"
	"github.com/tendant/voc-agent/internal/store"
	"github.com/tendant/voc-agent/internal/tools"
)

// Limits for one goal.
const (
	maxGoalSteps = 6
	maxGoalCalls = 10
	maxGoalTime  = 3 * time.Minute
	maxReplans   = 1
	// Reminders one task may create, unless the goal asks for reminders.
	maxGoalReminders, maxAskedReminders = 3, 5
)

// appleNotesIntent: Apple Notes tools are planned only when the user asks
// for them; otherwise "my notes" means voc's local notes.
var appleNotesIntent = regexp.MustCompile(`(?i)\bapple notes?\b|\bnotes app\b|\bin notes\b`)

// goalIntent: requests phrased as an outcome to achieve.
var goalIntent = regexp.MustCompile(`(?i)^(?:(?:please|ok|okay|hey victor|victor|can you|could you|i need you to|i want you to),? )*(?:get me ready for|help me (?:prepare|get ready|plan|organi[sz]e|get organi[sz]ed)|prepare (?:me )?for|take care of|sort out|get (?:everything|it all|things) (?:ready|done|sorted)|plan (?:my|the|a|out)|organi[sz]e (?:my|the)|set (?:me|things|everything) up for)\b`)

// resumeIntent: pick up the last interrupted task.
var resumeIntent = regexp.MustCompile(`(?i)\b(continue|resume|finish|pick up) (?:with )?(?:my |the )?(?:last |previous |unfinished )?(task|goal|plan|what you were doing)\b`)

// planAndDoTool lets the model hand a request to goal mode.
var planAndDoTool = llm.Tool{Type: "function", Function: llm.FunctionSpec{
	Name: "plan_and_do",
	Description: "Hand over a request that states an outcome needing several different steps you would have to work out yourself " +
		"(for example \"get me ready for Thursday's deploy\" or \"organize my week\"). Do not use it for a single action or a simple question.",
	Parameters: json.RawMessage(`{"type":"object","properties":{"goal":{"type":"string","description":"The outcome the user wants, in their words"}},"required":["goal"]}`),
}}

type goalPlan struct {
	CanDo   bool       `json:"can_do"`
	Reason  string     `json:"reason,omitempty"`
	Summary string     `json:"summary"`
	Steps   []goalStep `json:"steps"`
	Replans int        `json:"replans,omitempty"`
}

type goalStep struct {
	Tool   string     `json:"tool"`
	Intent string     `json:"intent"`
	Status string     `json:"status,omitempty"` // pending | running | done | failed | skipped
	Note   string     `json:"note,omitempty"`
	Calls  []goalCall `json:"calls,omitempty"`
}

type goalCall struct {
	Args   json.RawMessage `json:"args"`
	OK     bool            `json:"ok"`
	Result json.RawMessage `json:"result"`
}

// goalEnv is what a goal run needs from the turn it runs in.
type goalEnv struct {
	ctx    context.Context
	turn   uint32
	sink   Sink
	user   string
	notes  string                             // recalled local notes, as context
	say    func(sentences []string) string    // speak (claim-checked)
	run    func(tc llm.ToolCall) tools.Result // run a tool through the guards
	setKey func(key string)                   // idempotency key for following calls
	ok     func() map[string]bool             // tools that succeeded so far
}

func (s *Session) goalsEnabled() bool { return s.Tasks != nil && s.Tools != nil }

// planner returns the goal-mode model and whether to use its thinking.
func (s *Session) planner() (llm.Chat, bool) {
	if s.Planner != nil {
		return s.Planner, s.PlannerThinks && !s.PlanFast
	}
	return s.LLM, !s.PlanFast
}

// maxPlanThinking bounds planning with thinking; past it, plan without.
const maxPlanThinking = 45 * time.Second

// runGoal plans and carries out goal, or resumes task if given.
func (s *Session) runGoal(env goalEnv, goal string, resume *store.Task) {
	ctx, cancel := context.WithTimeout(env.ctx, maxGoalTime)
	defer cancel()

	var plan goalPlan
	var taskID int64
	if resume != nil {
		goal, taskID = resume.Goal, resume.ID
		if json.Unmarshal(resume.Plan, &plan) != nil || len(plan.Steps) == 0 {
			env.say([]string{"Sorry, I can't resume that task."})
			return
		}
		for i := range plan.Steps {
			if st := plan.Steps[i].Status; st != "done" && st != "skipped" {
				plan.Steps[i].Status = "pending" // includes a step cut off mid-way
			}
		}
	} else {
		env.say([]string{"Let me plan that."})
		p, err := s.plan(ctx, env, goal, nil)
		if err != nil {
			env.say([]string{"Sorry, I couldn't make a plan for that."})
			env.sink.Event(Event{Type: "error", Turn: env.turn, Error: "plan: " + err.Error()})
			return
		}
		plan = p
		if !plan.CanDo || len(plan.Steps) == 0 {
			why := strings.TrimSpace(plan.Reason)
			if why == "" {
				why = "I don't have the tools for that."
			}
			env.say([]string{"I can't do that on my own. " + why})
			return
		}
		b, _ := json.Marshal(plan)
		taskID, _ = s.Tasks.CreateTask(context.WithoutCancel(ctx), goal, "planned", b)
	}
	save := func(status string) {
		b, _ := json.Marshal(plan)
		s.Tasks.UpdateTask(context.WithoutCancel(ctx), taskID, status, b)
		env.sink.Event(Event{Type: "goal", Turn: env.turn, Text: status, Result: b, Name: goal})
	}
	save("planned")

	// Ask once before acting. Read-only plans just run.
	if s.planActs(plan) {
		b, _ := json.Marshal(plan)
		prompt := "Here's my plan: " + strings.TrimSuffix(plan.Summary, ".") + ". Should I go ahead?"
		if resume != nil {
			prompt = "I'll pick up where I left off: " + strings.TrimSuffix(plan.Summary, ".") + ". Should I go ahead?"
		}
		if s.Approver == nil {
			env.say([]string{"I need your OK before acting, but approvals aren't available."})
			save("cancelled")
			return
		}
		ok, err := s.Approver(ctx, env.turn, env.sink, tools.ApprovalRequest{
			Kind: "plan", ID: fmt.Sprintf("task-%d-%d", taskID, time.Now().UnixNano()), Prompt: prompt,
			Reason: goal, Plan: b,
		})
		if err != nil || !ok {
			save("cancelled")
			if ctx.Err() == nil {
				env.say([]string{"Okay, I won't do it."})
			}
			return
		}
		env.say([]string{"On it."})
	}

	status := s.execute(ctx, env, goal, taskID, &plan, save)
	save(status)
	if env.ctx.Err() != nil {
		return // interrupted: the journal says how far it got
	}
	s.report(ctx, env, goal, plan, status)
}

// planActs reports whether any step changes something (needs approval).
func (s *Session) planActs(p goalPlan) bool {
	ro := map[string]bool{}
	for _, t := range s.Tools.Available() {
		ro[t.Name] = t.ReadOnly
	}
	for _, st := range p.Steps {
		if st.Status != "done" && st.Status != "skipped" && !ro[st.Tool] {
			return true
		}
	}
	return false
}

// execute runs pending steps in order. It returns the final task status.
func (s *Session) execute(ctx context.Context, env goalEnv, goal string, taskID int64, plan *goalPlan, save func(string)) string {
	calls := 0
	reminderCap := maxGoalReminders
	if remindIntent.MatchString(goal) {
		reminderCap = maxAskedReminders
	}
	reminders := 0
	for _, st := range plan.Steps { // count ones made before a resume
		for _, c := range st.Calls {
			if st.Tool == "create_reminder" && c.OK {
				reminders++
			}
		}
	}
	for i := 0; i < len(plan.Steps); i++ {
		st := &plan.Steps[i]
		if st.Status == "done" || st.Status == "skipped" {
			continue
		}
		if ctx.Err() != nil {
			return map[bool]string{true: "interrupted", false: "failed"}[env.ctx.Err() != nil]
		}
		st.Status, st.Calls, st.Note = "running", nil, ""
		save("running")

		argsList, err := s.fillArgs(ctx, env, goal, *plan, i)
		if err != nil {
			st.Status, st.Note = "failed", "couldn't work out the details: "+err.Error()
		} else if len(argsList) == 0 {
			st.Status, st.Note = "skipped", "nothing to do"
		} else {
			okCount := 0
			for j, args := range argsList {
				if calls >= maxGoalCalls {
					st.Note = "stopped: too many actions for one task"
					break
				}
				if st.Tool == "create_reminder" && reminders >= reminderCap {
					st.Note = fmt.Sprintf("kept to %d reminders for one task", reminderCap)
					argsList = argsList[:j]
					break
				}
				calls++
				// Same key on a resume: a finished side effect isn't repeated.
				env.setKey(fmt.Sprintf("task%d.step%d.call%d", taskID, i, j))
				res := env.run(llm.ToolCall{Function: llm.FunctionCall{Name: st.Tool, Arguments: args}})
				st.Calls = append(st.Calls, goalCall{Args: args, OK: res.OK, Result: res.JSON})
				if res.OK {
					okCount++
					if st.Tool == "create_reminder" {
						reminders++
					}
				}
				if ctx.Err() != nil {
					break
				}
			}
			// Calls rejected for their arguments get one fix-up attempt.
			if okCount < len(argsList) && env.ctx.Err() == nil && calls < maxGoalCalls {
				var fb []string
				for _, c := range st.Calls {
					if !c.OK {
						fb = append(fb, fmt.Sprintf("args %s → %s", c.Args, c.Result))
					}
				}
				if fixed, err := s.fillArgs(ctx, env, goal, *plan, i, fb...); err == nil {
					for j, args := range fixed {
						if calls >= maxGoalCalls || (st.Tool == "create_reminder" && reminders >= reminderCap) {
							break
						}
						calls++
						env.setKey(fmt.Sprintf("task%d.step%d.fix%d", taskID, i, j))
						res := env.run(llm.ToolCall{Function: llm.FunctionCall{Name: st.Tool, Arguments: args}})
						st.Calls = append(st.Calls, goalCall{Args: args, OK: res.OK, Result: res.JSON})
						if res.OK {
							okCount++
							if st.Tool == "create_reminder" {
								reminders++
							}
						}
					}
				}
			}
			if env.ctx.Err() != nil {
				// Cut off mid-step: leave it unfinished so a resume redoes it.
				// Calls that already happened carry the same keys then, so they
				// aren't repeated.
				st.Status, st.Note = "interrupted", fmt.Sprintf("cut off after %d of %d", len(st.Calls), len(argsList))
				save("interrupted")
				return "interrupted"
			}
			switch {
			case okCount >= len(argsList):
				st.Status = "done"
			case okCount > 0:
				st.Status, st.Note = "done", fmt.Sprintf("%d of %d worked", okCount, len(argsList))
			default:
				st.Status = "failed"
			}
		}
		save("running")
		if env.ctx.Err() != nil {
			return "interrupted"
		}
		if st.Status == "failed" {
			if plan.Replans >= maxReplans {
				return "failed"
			}
			// Re-plan what's left in light of what happened.
			p, err := s.plan(ctx, env, goal, plan)
			if err != nil || !p.CanDo {
				return "failed"
			}
			plan.Replans++
			plan.Steps = append(plan.Steps[:i+1], p.Steps...)
			if s.planActs(goalPlan{Steps: p.Steps}) && !s.planActs(goalPlan{Steps: plan.Steps[:i+1]}) {
				// The new plan acts where the approved one didn't: stop
				// rather than act without asking.
				return "failed"
			}
		}
		if calls >= maxGoalCalls {
			return "failed"
		}
	}
	for _, st := range plan.Steps {
		if st.Status == "failed" {
			return "failed"
		}
	}
	return "done"
}

const plannerPrompt = `You plan tasks for a private voice assistant that runs on the user's computer.
Turn the user's goal into a short list of steps. Each step uses exactly one of the tools below.
Rules:
- 1 to %d steps. Fewer is better. No step may just talk to the user; the assistant reports at the end.
- Look things up before acting on them (for example search notes before making reminders about them).
- When the same tool is needed for several items, use ONE step that runs it several times, e.g. "create a reminder for each open question". Never repeat the same tool in consecutive steps.
- Reminders are for things the user must still do or decide (open questions, follow-ups), at most 3 unless the user asked for more. A list of many items belongs in one note if a note tool exists, not in many reminders.
- Don't add steps the goal doesn't need (for example listing reminders, or reading something twice).
- Use only the tools listed. If the goal can't be done with them, set can_do to false and give a short reason.
- summary: one short spoken sentence describing the plan, e.g. "I'll read your deployment notes, set a reminder for each open question, and save a checklist note."
- Never put private details into web searches or cloud questions.

Tools:
%s
The current local time is %s.`

// plan asks the local model (with thinking) for a plan, or for the
// remaining steps after a failure when done is given.
func (s *Session) plan(ctx context.Context, env goalEnv, goal string, done *goalPlan) (goalPlan, error) {
	infos := s.Tools.Available()
	var catalog strings.Builder
	var names []string
	for _, t := range infos {
		if t.Name == "ask_cloud" {
			continue // goal mode stays local; the user can ask the cloud directly
		}
		if strings.HasPrefix(t.Name, "apple_notes_") && !appleNotesIntent.MatchString(goal) {
			continue
		}
		kind := "acts"
		if t.ReadOnly {
			kind = "read-only"
		}
		fmt.Fprintf(&catalog, "- %s (%s): %s Arguments: %s\n", t.Name, kind, t.Description, compact(t.Schema, 400))
		names = append(names, t.Name)
	}
	if len(names) == 0 {
		return goalPlan{}, fmt.Errorf("no tools available")
	}
	enum, _ := json.Marshal(names)
	schema := json.RawMessage(fmt.Sprintf(`{"type":"object","properties":{
		"can_do":{"type":"boolean"},"reason":{"type":"string"},"summary":{"type":"string"},
		"steps":{"type":"array","maxItems":%d,"items":{"type":"object","properties":{
			"tool":{"type":"string","enum":%s},"intent":{"type":"string"}},"required":["tool","intent"]}}},
		"required":["can_do","summary","steps"]}`, maxGoalSteps, enum))

	user := "Goal: " + goal
	if env.notes != "" {
		user += "\n\nNotes from the user's files that may help:\n" + env.notes
	}
	if done != nil {
		b, _ := json.Marshal(done.Steps)
		user += "\n\nSteps so far, with results (the last one failed):\n" + compact(b, 3500) +
			"\n\nPlan only the remaining steps to still reach the goal, working around the failure. If it can't be done, set can_do to false."
	}
	msgs := []llm.Message{
		{Role: "system", Content: fmt.Sprintf(plannerPrompt, maxGoalSteps, catalog.String(), s.now().Format("Monday, January 2, 2006, 3:04 PM"))},
		{Role: "user", Content: user},
	}
	model, think := s.planner()
	var m llm.Message
	var err error
	if think {
		tctx, cancel := context.WithTimeout(ctx, maxPlanThinking)
		m, err = model.Chat(tctx, llm.Request{Messages: msgs, Think: true, Format: schema}, nil)
		cancel()
		if err != nil && ctx.Err() == nil {
			think = false // thinking ran long: plan without it
		}
	}
	if !think {
		m, err = model.Chat(ctx, llm.Request{Messages: msgs, Format: schema}, nil)
	}
	if err != nil {
		return goalPlan{}, err
	}
	var p goalPlan
	if err := json.Unmarshal([]byte(m.Content), &p); err != nil {
		return goalPlan{}, fmt.Errorf("bad plan: %w", err)
	}
	known := map[string]bool{}
	for _, n := range names {
		known[n] = true
	}
	var steps []goalStep
	for _, st := range p.Steps {
		if !known[st.Tool] || strings.TrimSpace(st.Intent) == "" {
			continue
		}
		// Small planners repeat a tool step by step ("remind A", "remind B");
		// merge those into one step that runs the tool several times.
		if n := len(steps); n > 0 && steps[n-1].Tool == st.Tool {
			steps[n-1].Intent += "; " + st.Intent
			continue
		}
		if len(steps) < maxGoalSteps {
			st.Status = "pending"
			steps = append(steps, st)
		}
	}
	p.Steps = steps
	return p, nil
}

const fillPrompt = `You fill in the details for one step of a task a voice assistant is carrying out for the user.
Return JSON {"calls": [...]}: one arguments object per call of the tool %s that this step needs (at most 5), or an empty list if, given the results so far, there is nothing to do.
Use only facts from the goal, the results so far and the notes. Don't invent names, dates or items.
For reminders, give "when" in plain words like "tomorrow at 9am" or "Thursday at 5pm".
If the goal itself says when (for example "at 8 pm", "tomorrow"), use exactly that; the user's words override anything in the notes.
The current local time is %s.`

// fillArgs asks the model for the concrete argument objects of step i.
// feedback, if set, describes why the previous arguments failed.
func (s *Session) fillArgs(ctx context.Context, env goalEnv, goal string, plan goalPlan, i int, feedback ...string) ([]json.RawMessage, error) {
	st := plan.Steps[i]
	var schema json.RawMessage
	var desc string
	for _, t := range s.Tools.Available() {
		if t.Name == st.Tool {
			schema, desc = t.Schema, t.Description
		}
	}
	if schema == nil {
		return nil, fmt.Errorf("%s isn't available right now", st.Tool)
	}
	var b strings.Builder
	fmt.Fprintf(&b, "Goal: %s\n\nPlan:\n", goal)
	for j, p := range plan.Steps {
		mark := "  "
		if j == i {
			mark = "->"
		}
		fmt.Fprintf(&b, "%s %d. [%s] %s (%s)\n", mark, j+1, p.Tool, p.Intent, cmpOr(p.Status, "pending"))
	}
	fmt.Fprintf(&b, "\nThis step: %s\nTool %s: %s\n", st.Intent, st.Tool, desc)
	var results []map[string]any
	for _, p := range plan.Steps[:i] {
		for _, c := range p.Calls {
			results = append(results, map[string]any{"tool": p.Tool, "args": c.Args, "ok": c.OK, "result": c.Result})
		}
	}
	if len(results) > 0 {
		r, _ := json.Marshal(results)
		fmt.Fprintf(&b, "\nResults so far:\n%s\n", compact(r, 5000))
	}
	if env.notes != "" {
		fmt.Fprintf(&b, "\nNotes from the user's files:\n%s\n", env.notes)
	}
	if len(feedback) > 0 {
		fmt.Fprintf(&b, "\nYour previous arguments for this step failed:\n%s\nFix them. Return only the calls that still need doing.\n", strings.Join(feedback, "\n"))
	}
	format := json.RawMessage(fmt.Sprintf(`{"type":"object","properties":{"calls":{"type":"array","maxItems":5,"items":%s}},"required":["calls"]}`, schema))
	model, _ := s.planner()
	m, err := model.Chat(ctx, llm.Request{Format: format, Messages: []llm.Message{
		{Role: "system", Content: fmt.Sprintf(fillPrompt, st.Tool, s.now().Format("Monday, January 2, 2006, 3:04 PM"))},
		{Role: "user", Content: b.String()},
	}}, nil)
	if err != nil {
		return nil, err
	}
	var out struct {
		Calls []json.RawMessage `json:"calls"`
	}
	if err := json.Unmarshal([]byte(m.Content), &out); err != nil {
		return nil, fmt.Errorf("bad arguments: %w", err)
	}
	if len(out.Calls) > 5 {
		out.Calls = out.Calls[:5]
	}
	return out.Calls, nil
}

const reportPrompt = `You are a voice assistant telling the user, out loud, what you just did for their goal.
Speak in the first person ("I set two reminders…"). Write 1 to 3 short plain sentences from the step results only. Say plainly what failed or was skipped. Don't list every detail, and don't offer more help.`

// report speaks a summary built from what actually happened.
func (s *Session) report(ctx context.Context, env goalEnv, goal string, plan goalPlan, status string) {
	b, _ := json.Marshal(plan.Steps)
	model, _ := s.planner()
	m, err := model.Chat(context.WithoutCancel(ctx), llm.Request{Messages: []llm.Message{
		{Role: "system", Content: reportPrompt},
		{Role: "user", Content: fmt.Sprintf("Goal: %s\nOutcome: %s\nSteps and results:\n%s", goal, status, compact(b, 6000))},
	}}, nil)
	text := ""
	if err == nil {
		text = strings.TrimSpace(m.Content)
	}
	ch := &Chunker{MinLen: 12}
	if text == "" || env.say(append(ch.Push(text), ch.Flush()...)) != "" {
		// No report, or it claimed something that didn't happen: say only
		// what the journal shows.
		env.say([]string{plainReport(plan, status)})
	}
}

// plainReport is the fallback summary straight from the step statuses.
func plainReport(p goalPlan, status string) string {
	done, failed := 0, 0
	for _, st := range p.Steps {
		switch st.Status {
		case "done":
			done++
		case "failed":
			failed++
		}
	}
	switch {
	case status == "done":
		return fmt.Sprintf("Done: I finished all %d steps.", len(p.Steps))
	case failed > 0:
		return fmt.Sprintf("I finished %d of %d steps; one step didn't work, so I stopped. You can see the details on screen.", done, len(p.Steps))
	}
	return fmt.Sprintf("I finished %d of %d steps.", done, len(p.Steps))
}

func compact(b []byte, max int) string {
	s := string(b)
	if len(s) > max {
		return s[:max] + " …"
	}
	return s
}

func cmpOr(a, b string) string {
	if a != "" {
		return a
	}
	return b
}

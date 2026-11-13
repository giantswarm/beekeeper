package superloop

import (
	"fmt"
	"slices"
	"strings"
	"time"
)

// round is a review round the loop started: the head it reviews and when,
// until when the next round started (zero for the last), and the heads the
// rounds before it reviewed.
type round struct {
	head    string
	at      time.Time
	until   time.Time
	earlier []string
	// later are the heads the rounds after it opened on.
	later []string
}

// actedHead is a head the loop acted on, and when.
type actedHead struct {
	head string
	at   time.Time
}

// question is a pull request whose ownership the guide's person was asked
// about: when, and the person's answer once there is one.
type question struct {
	at     time.Time
	answer *Answer
}

// ledger is what the taken actions say.
type ledger struct {
	keys map[string]Taken
	// rounds are the review rounds each pull request had, oldest first,
	// ended the ones merged or closed.
	rounds   map[string][]round
	ended    map[string]bool
	reviewer map[string]string // a pull request's worker, from its last review
	// goAgent is the worker the go for a pull request was given to; goHead
	// is the last head given a go and not superseded by a later review,
	// forward or rework; acts are when the loop acted on a pull request (a
	// review, go, forward, rework or end), oldest first.
	goAgent map[string]string
	goHead  map[string]string
	acts    map[string][]time.Time
	// asked are the pull requests whose ownership the person was asked
	// about, overridden the ones they let the desk take over.
	asked      map[string]question
	overridden map[string]bool
	// red are the checks the loop woke a worker for, by redKey.
	red map[string]bool
	// actedOn are the heads the loop gave a go, a red wake or a rework
	// for, by ref, oldest first: each was the pull request's head then.
	actedOn map[string][]actedHead
	// episode identifies a new tracking episode after a close.
	episode map[string]string
}

func newLedger(taken []Taken, answers []Answer) ledger {
	l := ledger{keys: map[string]Taken{}, rounds: map[string][]round{}, ended: map[string]bool{}, reviewer: map[string]string{},
		goAgent: map[string]string{}, goHead: map[string]string{}, acts: map[string][]time.Time{}, asked: map[string]question{}, overridden: map[string]bool{}, red: map[string]bool{}, actedOn: map[string][]actedHead{}, episode: map[string]string{}}
	taken, l.episode = episodeTaken(taken)
	for _, t := range taken {
		l.keys[t.Key] = t
		if slices.Contains([]string{KindReview, KindGo, KindForward, KindRework, KindAnswer, KindEnd, KindLeave, KindOwner}, t.Kind) {
			l.acts[t.Ref] = append(l.acts[t.Ref], t.At)
		}
		if head := KeyHead(t.Key); head != "" && slices.Contains([]string{KindGo, KindRed, KindRework}, t.Kind) {
			l.actedOn[t.Ref] = append(l.actedOn[t.Ref], actedHead{head: head, at: t.At})
		}
		switch t.Kind {
		case KindReview:
			head := KeyHead(t.Key)
			l.rounds[t.Ref] = append(l.rounds[t.Ref], round{head: head, at: t.At})
			l.reviewer[t.Ref] = t.Agent
			// A legacy exhausted-round go may predate this head's review.
			// Its refusal must not suppress the go after actual approval.
			delete(l.keys, goKey(t.Ref, head))
			delete(l.goHead, t.Ref)
		case KindRework:
			delete(l.goHead, t.Ref)
		case KindEnd:
			l.ended[t.Ref] = true
		case KindOwnership:
			l.asked[t.Ref] = question{at: t.At}
		case KindOverride:
			l.overridden[t.Ref] = true
		case KindRed:
			// "red <ref>@<head> <parked>: <check>; <check>…"
			if prefix, checks, ok := strings.Cut(t.Key, ": "); ok {
				for name := range strings.SplitSeq(checks, redSep) {
					l.red[prefix+": "+name] = true
				}
			}
		case KindGo, KindForward, KindOwner:
			if t.Kind == KindGo {
				if head := KeyHead(t.Key); head != "" {
					l.goHead[t.Ref] = head
				}
			} else {
				delete(l.goHead, t.Ref)
			}
			if t.Agent != "" {
				l.reviewer[t.Ref] = t.Agent
				if t.Kind == KindGo {
					l.goAgent[t.Ref] = t.Agent
				}
			}
		}
	}
	for _, rs := range l.rounds {
		for i := range rs {
			if i+1 < len(rs) {
				rs[i].until = rs[i+1].at
			}
			for _, rd := range rs[:i] {
				rs[i].earlier = append(rs[i].earlier, rd.head)
			}
			for _, rd := range rs[i+1:] {
				rs[i].later = append(rs[i].later, rd.head)
			}
		}
	}
	// The person's answer to a question about a pull request's ownership
	// is the last one recorded after it to the loop's question.
	for _, an := range answers {
		q, ok := l.asked[an.Ref]
		if ok && an.At.After(q.at) && strings.HasPrefix(an.Question, ownershipPrefix) {
			q.answer = &an
			l.asked[an.Ref] = q
		}
	}
	return l
}

// episodeTag gives every action after a reopen a distinct durable key,
// including another review of the same head and another end of the same ref.
const episodeTag = " reopened "

// episodeTaken retains only the latest tracking episode of each pull request.
// Legacy keys belong to its original episode. The planner uses keys without
// the suffix internally; emitted actions restore it before execution.
func episodeTaken(taken []Taken) ([]Taken, map[string]string) {
	episodes := map[string]string{}
	for _, t := range taken {
		if _, episode := splitEpisode(t.Key); episode != "" && t.Ref != "" {
			episodes[t.Ref] = episode
		}
	}
	var active []Taken
	for _, t := range taken {
		if t.Ref == "" {
			active = append(active, t)
			continue
		}
		key, episode := splitEpisode(t.Key)
		if t.Ref != "" && episode != episodes[t.Ref] {
			continue
		}
		t.Key = key
		active = append(active, t)
	}
	return active, episodes
}

// splitEpisode recognizes only a trailing episode timestamp. Check names and
// other key text may themselves contain episodeTag.
func splitEpisode(key string) (string, string) {
	if i := strings.LastIndex(key, episodeTag); i >= 0 {
		episode := key[i+len(episodeTag):]
		if _, err := time.Parse(time.RFC3339Nano, episode); err == nil {
			return key[:i], episode
		}
	}
	return key, ""
}

// BaseActionKey removes a reopened episode suffix for action comparisons.
// Durable claim and completion records must still use the full key.
func BaseActionKey(key string) string {
	base, _ := splitEpisode(key)
	return base
}

// reopen discards the closed episode only after GitHub reports OPEN.
func (l ledger) reopen(ref string) {
	l.episode[ref] = l.keys[endKey(ref)].At.UTC().Format(time.RFC3339Nano)
	for key, t := range l.keys {
		if t.Ref == ref {
			delete(l.keys, key)
		}
	}
	for key := range l.red {
		if strings.HasPrefix(key, "red "+ref+"@") {
			delete(l.red, key)
		}
	}
	delete(l.rounds, ref)
	delete(l.ended, ref)
	delete(l.reviewer, ref)
	delete(l.goAgent, ref)
	delete(l.goHead, ref)
	delete(l.acts, ref)
	delete(l.actedOn, ref)
	delete(l.asked, ref)
	delete(l.overridden, ref)
}

func (l ledger) has(key string) bool { _, ok := l.keys[key]; return ok }

// pushed reports whether review r of p is on a head pushed while a round
// ran, by its worker or by its reviewer fixing its findings: the pull
// request's head now, which no rule lets go back to an older one; a head
// whose findings the loop forwarded already; the head GitHub attached the
// review to itself, the pull request's head when it was posted; or a head
// the loop gave a go, a red wake or a rework for once the review was there.
// So a round's verdict stays its verdict once the head moves on: the one
// desk review is never lost to a later push
// (https://github.com/giantswarm/beekeeper/issues/3861).
func (l ledger) pushed(p Pull) func(Review) bool {
	return func(r Review) bool {
		return sameHead(r.Head, p.Head) || l.has(forwardKey(p.Ref, r.ID)) || sameHead(r.Commit, r.Head) ||
			slices.ContainsFunc(l.actedOn[p.Ref], func(a actedHead) bool {
				return sameHead(a.head, r.Head) && !a.at.Before(r.At.Add(-verdictSlack))
			})
	}
}

// actedAfter reports whether the loop acted on ref after at: a round's
// verdict is acted on once anything follows the round's start.
func (l ledger) actedAfter(ref string, at time.Time) bool {
	return slices.ContainsFunc(l.acts[ref], func(t time.Time) bool { return t.After(at) })
}

// workerDue reports whether the worker the go for ref was given to waits
// for its post-merge report: a beekeeper-started session still on the
// roster and not retired yet. A park on a release, timer or note keeps
// the worker reachable too.
func (l ledger) workerDue(ref string, workers []Worker) bool {
	agent := l.goAgent[ref]
	return agent != "" && !l.has(retireKey(ref, retiredWorker)) &&
		slices.ContainsFunc(workers, func(w Worker) bool {
			return w.Name == agent && w.Started
		})
}

// ReviewerName is the roster name of the reviewer of round rd of ref
// (owner/repo#n): "Review <repo> <n> r<rd>".
func ReviewerName(ref string, rd int) string {
	repo, n, _ := strings.Cut(ref, "#")
	_, name, _ := strings.Cut(repo, "/")
	return fmt.Sprintf("Review %s %s r%d", name, n, rd)
}

// PullWorkerName is the default roster name of the worker that owns ref
// (owner/repo#n): "<repo> <n>".
func PullWorkerName(ref string) string {
	repo, n, _ := strings.Cut(ref, "#")
	_, name, _ := strings.Cut(repo, "/")
	if name == "" || n == "" {
		return ""
	}
	return name + " " + n
}

// Key renderings.
func reviewKey(ref, head string) string    { return fmt.Sprintf("review %s@%s", ref, head) }
func goKey(ref, head string) string        { return fmt.Sprintf("go %s@%s", ref, head) }
func forwardKey(ref, review string) string { return fmt.Sprintf("forward %s %s", ref, review) }

func reworkKey(ref, head string, parked time.Time) string {
	return fmt.Sprintf("rework %s@%s %s", ref, head, parked.UTC().Format(time.RFC3339Nano))
}

func ownerKey(ref, head string, parked time.Time) string {
	return fmt.Sprintf("owner %s@%s %s", ref, head, parked.UTC().Format(time.RFC3339Nano))
}

func answerKey(ref string, note int) string { return fmt.Sprintf("answer %s #%d", ref, note) }
func endKey(ref string) string              { return "end " + ref }
func intakeKey(ref string) string           { return "intake " + strings.ToLower(ref) }

// retryKey is the key of round rd of ref's head after an earlier round's
// reviewer left that head without a verdict: reviewKey names the head's
// first round only.
func retryKey(ref, head string, rd int) string {
	return fmt.Sprintf("review %s@%s r%d", ref, head, rd)
}

// redKey is the key of the wake that tells the red checks names of ref's
// head to the park of parked; a single check's redKey is its own mark.
func redKey(ref, head string, parked time.Time, names ...string) string {
	return fmt.Sprintf("red %s@%s %s: %s", ref, head, parked.UTC().Format(time.RFC3339Nano), strings.Join(names, redSep))
}

// redSep separates the checks of one red wake's key.
const redSep = "; "

// redName is a check's name as a key holds it: on one line, without the
// brackets that end a key or the separator of its checks.
func redName(name string) string {
	name = strings.NewReplacer("[", "(", "]", ")", ";", ",").Replace(name)
	return strings.Join(strings.Fields(name), " ")
}

func eventKey(kind string, e Event) string {
	return fmt.Sprintf("%s %s@%s %s", kind, e.Verb, e.At.UTC().Format(time.RFC3339Nano), e.Agent)
}

// retireKey is the done of the reviewer of a round or the worker of ref.
func retireKey(ref, who string) string { return fmt.Sprintf("done %s %s", ref, who) }

// retiredWorker names the worker in its retirement's key.
const retiredWorker = "worker"

// leftServing is the reason of the leave that keeps a merged pull
// request's worker on the roster for the open work it still serves.
const leftServing = "serving"

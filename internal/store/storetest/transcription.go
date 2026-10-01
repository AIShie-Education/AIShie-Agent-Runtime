package storetest

import (
	"errors"
	"reflect"
	"slices"
	"testing"
	"time"

	"github.com/AIShie-Education/AIShie-Agent-Runtime/internal/store"
)

// serviceToken is the transcriber's service credential, as the vault would
// seal it.
func serviceToken(id string) store.Secret {
	return sealed(id, store.SiteTenantID, store.SecretCoreToken)
}

// credential is the credential of the token id, as the API would keep it.
func credential(id string) store.TranscriptionCredential {
	return store.TranscriptionCredential{SecretID: id, Hint: "aissvc_ab12cd34ef56…", CredentialID: "cred-1", Tested: true,
		SetBy: "admin-1", SetAt: at(time.Hour)}
}

func getCredential(t *testing.T, s store.Store) *store.TranscriptionCredential {
	t.Helper()
	c, err := s.TranscriptionCredential(t.Context())
	if err != nil {
		t.Fatalf("TranscriptionCredential: %v", err)
	}
	return c
}

// job is a job of version v, started at when.
func job(id, v string, when time.Time) store.TranscriptionJob {
	return store.TranscriptionJob{ID: id, VersionID: v, DocumentID: "doc-" + v, CourseID: "course-1", LeaseID: "lease-" + v,
		Status: store.JobWorking, Attempt: 1, ContentType: "application/pdf", ByteSize: 20480, Worker: "w1", StartedAt: when,
		HeartbeatAt: when}
}

func putJob(t *testing.T, s store.Store, j store.TranscriptionJob) *store.TranscriptionJob {
	t.Helper()
	got, err := s.PutTranscriptionJob(t.Context(), j)
	if err != nil {
		t.Fatalf("PutTranscriptionJob(%s): %v", j.ID, err)
	}
	return got
}

func jobs(t *testing.T, s store.Store, q store.JobQuery) []store.TranscriptionJob {
	t.Helper()
	if q.Limit == 0 {
		q.Limit = 50
	}
	got, err := s.TranscriptionJobs(t.Context(), q)
	if err != nil {
		t.Fatalf("TranscriptionJobs(%+v): %v", q, err)
	}
	return got
}

func jobIDs(js []store.TranscriptionJob) []string {
	out := make([]string, len(js))
	for i, j := range js {
		out[i] = j.ID
	}
	return out
}

func ptr[T any](v T) *T { return &v }

func testTranscription(t *testing.T, open Opener) {
	t.Run("a credential kept with its token, noted, replaced and forgotten", func(t *testing.T) {
		s, ctx := open(t), t.Context()
		if _, err := s.TranscriptionCredential(ctx); !errors.Is(err, store.ErrNotFound) {
			t.Fatalf("no credential yet: %v", err)
		}
		r := rev(t, s)
		if err := s.PutTranscriptionCredential(ctx, credential("sec_svc_1"), serviceToken("sec_svc_1")); err != nil {
			t.Fatal(err)
		}
		if rev(t, s) == r {
			t.Error("giving the credential moved no revision on")
		}
		got := getCredential(t, s)
		want := credential("sec_svc_1")
		sameTime(t, "set_at", got.SetAt, want.SetAt)
		got.SetAt, want.SetAt = time.Time{}, time.Time{}
		if !reflect.DeepEqual(*got, want) {
			t.Errorf("the credential:\n got %+v\nwant %+v", *got, want)
		}
		sameSecret(t, *getSecret(t, s, "sec_svc_1"), serviceToken("sec_svc_1"))
		if err := s.DeleteSecret(ctx, "sec_svc_1"); !errors.Is(err, store.ErrInUse) {
			t.Errorf("deleting the credential's secret: %v, want ErrInUse", err)
		}

		// Notes move nothing on, and a refusal is kept until Core takes it
		// again.
		r = rev(t, s)
		if err := s.NoteTranscriptionCredential(ctx, "sec_svc_1", true, at(2*time.Hour), ""); err != nil {
			t.Fatal(err)
		}
		if c := getCredential(t, s); c.LastOKAt == nil || !c.LastOKAt.Equal(us(at(2*time.Hour))) || c.RejectedAt != nil || c.LastError != "" {
			t.Errorf("taken: %+v", c)
		}
		if err := s.NoteTranscriptionCredential(ctx, "sec_svc_1", false, at(3*time.Hour), "Core answered 401"); err != nil {
			t.Fatal(err)
		}
		if c := getCredential(t, s); c.RejectedAt == nil || !c.RejectedAt.Equal(us(at(3*time.Hour))) || c.LastError != "Core answered 401" ||
			c.LastOKAt == nil {
			t.Errorf("refused: %+v", c)
		}
		if err := s.NoteTranscriptionCredential(ctx, "sec_other", true, time.Time{}, ""); !errors.Is(err, store.ErrNotFound) {
			t.Errorf("a note of another credential: %v", err)
		}
		if got := rev(t, s); got != r {
			t.Errorf("notes moved the revision from %d to %d", r, got)
		}

		// Replaced: the one before goes, secret and notes.
		next := credential("sec_svc_2")
		next.CredentialID, next.Tested, next.SetBy, next.SetAt = "", false, "admin-2", time.Time{}
		before := time.Now()
		if err := s.PutTranscriptionCredential(ctx, next, serviceToken("sec_svc_2")); err != nil {
			t.Fatal(err)
		}
		got = getCredential(t, s)
		recent(t, "set_at", got.SetAt, before, time.Now())
		if got.SecretID != "sec_svc_2" || got.CredentialID != "" || got.Tested || got.SetBy != "admin-2" || got.LastOKAt != nil ||
			got.RejectedAt != nil || got.LastError != "" {
			t.Errorf("replaced: %+v", got)
		}
		missingSecret(t, s, "sec_svc_1")
		if rev(t, s) == r {
			t.Error("replacing the credential moved no revision on")
		}

		r = rev(t, s)
		if err := s.DeleteTranscriptionCredential(ctx); err != nil {
			t.Fatal(err)
		}
		if _, err := s.TranscriptionCredential(ctx); !errors.Is(err, store.ErrNotFound) {
			t.Errorf("after forgetting it: %v", err)
		}
		missingSecret(t, s, "sec_svc_2")
		if rev(t, s) == r {
			t.Error("forgetting the credential moved no revision on")
		}
		r = rev(t, s)
		if err := s.DeleteTranscriptionCredential(ctx); err != nil {
			t.Errorf("forgetting none: %v", err)
		}
		if got := rev(t, s); got != r {
			t.Errorf("forgetting none moved the revision from %d to %d", r, got)
		}
	})

	t.Run("a credential refused, and nothing of it kept", func(t *testing.T) {
		s, ctx := open(t), t.Context()
		if err := s.PutTranscriptionCredential(ctx, credential("sec_svc_1"), serviceToken("sec_svc_1")); err != nil {
			t.Fatal(err)
		}
		putSecret(t, s, serviceToken("sec_loose"))
		r := rev(t, s)
		for _, c := range []struct {
			name  string
			cred  store.TranscriptionCredential
			token store.Secret
		}{
			{"a token it does not refer to", credential("sec_svc_2"), serviceToken("sec_svc_3")},
			{"a model key", credential("sec_svc_2"), sealed("sec_svc_2", store.SiteTenantID, store.SecretModelKey)},
			{"a token of the school's", credential("sec_svc_2"), sealed("sec_svc_2", store.SchoolTenantID, store.SecretCoreToken)},
			{"no one who set it", func() store.TranscriptionCredential { c := credential("sec_svc_2"); c.SetBy = ""; return c }(),
				serviceToken("sec_svc_2")},
			{"a secret id taken", credential("sec_loose"), serviceToken("sec_loose")},
		} {
			if err := s.PutTranscriptionCredential(ctx, c.cred, c.token); err == nil {
				t.Errorf("%s: kept", c.name)
			}
			if c.token.ID != "sec_loose" {
				missingSecret(t, s, c.token.ID)
			}
		}
		if got := rev(t, s); got != r {
			t.Errorf("refused credentials moved the revision from %d to %d", r, got)
		}
		if c := getCredential(t, s); c.SecretID != "sec_svc_1" {
			t.Errorf("the credential after the refusals: %+v", c)
		}
		getSecret(t, s, "sec_svc_1")
	})

	t.Run("jobs kept in their places, listed newest first by status, and paged", func(t *testing.T) {
		s := open(t)
		a := putJob(t, s, job("trj_a", "v1", at(time.Minute)))
		b := putJob(t, s, job("trj_b", "v2", at(2*time.Minute)))
		c := putJob(t, s, job("trj_c", "v3", at(3*time.Minute)))
		if a.Seq < 1 || b.Seq <= a.Seq || c.Seq <= b.Seq {
			t.Fatalf("places: %d, %d, %d", a.Seq, b.Seq, c.Seq)
		}
		done := *a
		// A file of a version of several, by its id and place.
		done.FileID, done.Position = "file-2", 2
		finished := at(10 * time.Minute)
		done.Status, done.Pages, done.PagesSent, done.Offer, done.Model = store.JobDone, ptr(12), 12, "flash", "gemini-flash-lite"
		done.ModelCalls, done.CostPUSD, done.InputTokens, done.OutputTokens = 2, ptr(int64(4_000_000)), 3100, 5200
		done.HeartbeatAt, done.FinishedAt = at(9*time.Minute), &finished
		got := putJob(t, s, done)
		sameTime(t, "finished_at", *got.FinishedAt, finished)
		sameTime(t, "started_at", got.StartedAt, at(time.Minute))
		sameTime(t, "heartbeat_at", got.HeartbeatAt, at(9*time.Minute))
		want := done
		for _, g := range []*store.TranscriptionJob{got, &want} {
			g.StartedAt, g.HeartbeatAt, g.FinishedAt = time.Time{}, time.Time{}, nil
		}
		if !reflect.DeepEqual(*got, want) {
			t.Errorf("the job done:\n got %+v\nwant %+v", *got, want)
		}
		skipped := *b
		skipped.Status, skipped.Reason, skipped.Pages = store.JobSkipped, "too_many_pages", ptr(400)
		putJob(t, s, skipped)

		if all := jobs(t, s, store.JobQuery{}); !slices.Equal(jobIDs(all), []string{"trj_c", "trj_b", "trj_a"}) ||
			all[2].Seq != a.Seq || all[2].Status != store.JobDone || *all[1].Pages != 400 || all[2].FileID != "file-2" ||
			all[2].Position != 2 || all[1].FileID != "" || all[1].Position != 0 {
			t.Errorf("every job: %+v", all)
		}
		if got := jobs(t, s, store.JobQuery{Status: store.JobDone}); !slices.Equal(jobIDs(got), []string{"trj_a"}) {
			t.Errorf("the jobs done: %v", jobIDs(got))
		}
		page := jobs(t, s, store.JobQuery{Limit: 2})
		rest := jobs(t, s, store.JobQuery{Limit: 2, Before: page[1].Seq})
		if !slices.Equal(jobIDs(page), []string{"trj_c", "trj_b"}) || !slices.Equal(jobIDs(rest), []string{"trj_a"}) {
			t.Errorf("paged: %v then %v", jobIDs(page), jobIDs(rest))
		}
		for _, q := range []store.JobQuery{{Limit: 0}, {Limit: store.MaxJobPage + 1}, {Status: "pending", Limit: 1}} {
			if _, err := s.TranscriptionJobs(t.Context(), q); err == nil {
				t.Errorf("TranscriptionJobs(%+v) was taken", q)
			}
		}
		for _, bad := range []store.TranscriptionJob{
			job("job_1", "v9", at(0)),
			job("trj_x", "", at(0)),
			func() store.TranscriptionJob { j := job("trj_x", "v9", at(0)); j.Status = "pending"; return j }(),
			func() store.TranscriptionJob { j := job("trj_x", "v9", at(0)); j.PagesSent = -1; return j }(),
			func() store.TranscriptionJob { j := job("trj_x", "v9", at(0)); j.CostPUSD = ptr(int64(-1)); return j }(),
			func() store.TranscriptionJob { j := job("trj_x", "v9", at(0)); j.Position = -1; return j }(),
			func() store.TranscriptionJob {
				j := job("trj_x", "v9", at(0))
				j.Reason = string(make([]rune, 501))
				return j
			}(),
		} {
			if _, err := s.PutTranscriptionJob(t.Context(), bad); err == nil {
				t.Errorf("PutTranscriptionJob(%+v) was taken", bad)
			}
		}
		if n := len(jobs(t, s, store.JobQuery{})); n != 3 {
			t.Errorf("%d jobs after the refusals", n)
		}
	})

	t.Run("the day summed, jobs left working interrupted, and old ones pruned", func(t *testing.T) {
		s, ctx := open(t), t.Context()
		old := job("trj_old", "v0", at(-time.Hour))
		old.Status, old.PagesSent, old.CostPUSD = store.JobDone, 40, ptr(int64(9))
		putJob(t, s, old)
		for _, j := range []store.TranscriptionJob{
			func() store.TranscriptionJob {
				j := job("trj_1", "v1", at(time.Minute))
				j.Status, j.PagesSent, j.CostPUSD = store.JobDone, 12, ptr(int64(300))
				return j
			}(),
			func() store.TranscriptionJob {
				j := job("trj_2", "v2", at(2*time.Minute))
				j.Status, j.PagesSent = store.JobDone, 3
				return j
			}(),
			func() store.TranscriptionJob {
				j := job("trj_3", "v3", at(3*time.Minute))
				j.Status, j.Reason, j.PagesSent, j.CostPUSD = store.JobFailed, "model_error", 10, ptr(int64(50))
				return j
			}(),
			func() store.TranscriptionJob {
				j := job("trj_4", "v4", at(4*time.Minute))
				j.Status, j.Reason = store.JobSkipped, "quota_exhausted"
				return j
			}(),
			job("trj_5", "v5", at(5*time.Minute)),
		} {
			putJob(t, s, j)
		}
		day, err := s.TranscriptionDay(ctx, at(0))
		if err != nil {
			t.Fatal(err)
		}
		if want := (store.TranscriptionDay{Pages: 25, Documents: 2, Failed: 1, Skipped: 1, CostPUSD: 350}); day != want {
			t.Errorf("the day: %+v, want %+v", day, want)
		}

		// trj_5 was last held at 5 minutes: gone, with its worker.
		n, err := s.InterruptTranscriptionJobs(ctx, at(6*time.Minute), at(20*time.Minute))
		if err != nil || n != 1 {
			t.Fatalf("InterruptTranscriptionJobs = %d, %v", n, err)
		}
		if got := jobs(t, s, store.JobQuery{Status: store.JobDropped}); len(got) != 1 || got[0].ID != "trj_5" ||
			got[0].Reason != store.ReasonInterrupted || got[0].FinishedAt == nil || !got[0].FinishedAt.Equal(us(at(20*time.Minute))) {
			t.Errorf("the job interrupted: %+v", got)
		}
		if n, err := s.InterruptTranscriptionJobs(ctx, at(6*time.Minute), time.Time{}); err != nil || n != 0 {
			t.Errorf("again: %d, %v", n, err)
		}

		n, err = s.PruneTranscriptionJobs(ctx, at(0))
		if err != nil || n != 1 {
			t.Fatalf("PruneTranscriptionJobs = %d, %v", n, err)
		}
		if got := jobs(t, s, store.JobQuery{}); len(got) != 5 || slices.Contains(jobIDs(got), "trj_old") {
			t.Errorf("after the pruning: %v", jobIDs(got))
		}
	})

	t.Run("the transcriber's calls in the ledger: the school's key's alone", func(t *testing.T) {
		s, ctx := open(t), t.Context()
		day := at(time.Hour)
		yuki := scope{agent: "agt_y", tenant: "ten_yuki", course: "c1", opener: "m1", key: "school"}
		tx := func(id string, cost int64, version string) store.LLMCall {
			c := call(id, day, scope{key: "school"}, cost)
			c.Kind, c.MemberID, c.ConversationID, c.MessageID, c.PriceVersion = store.CallTranscription, "", "", "", version
			c.Provider, c.Model = "gemini", "gemini-flash-lite"
			return c
		}
		record(t, s, []store.LLMCall{call("c1", day, yuki, 100), tx("t1", 40, "v1/flash"), tx("t2", 0, "")},
			[]store.AnswerRecord{answerRecord("a1", day, yuki, true, 100)})
		if got := spend(t, s, store.SpendScope{KeySource: "school"}, at(0)); got != (store.Spend{Answers: 1, CostPUSD: 140}) {
			t.Errorf("the school's key's day: %+v", got)
		}
		for _, sc := range []store.SpendScope{{TenantID: "ten_yuki", KeySource: "school"}, {AgentID: "agt_y"}} {
			if got := spend(t, s, sc, at(0)); got != (store.Spend{Answers: 1, CostPUSD: 100}) {
				t.Errorf("Spend(%+v) = %+v: the transcriber's counted", sc, got)
			}
		}
		if got, err := s.TenantUsage(ctx, "school", at(0), at(2*time.Hour)); err != nil || len(got) != 1 || got[0].TenantID != "ten_yuki" ||
			got[0].ModelCalls != 1 {
			t.Errorf("TenantUsage = %+v, %v", got, err)
		}
		report := func(group string) []store.CostRow {
			t.Helper()
			rows, err := s.CostReport(ctx, store.CostQuery{Group: group, Since: at(0), Until: at(2 * time.Hour), Limit: 100})
			if err != nil {
				t.Fatal(err)
			}
			return rows
		}
		wantTx := store.CostCalls{Calls: 2, Unpriced: 1, InputTokens: 2400, CacheReadTokens: 2000, OutputTokens: 420, CostPUSD: 40}
		if total := report(store.CostByTotal); len(total) != 1 || total[0].ModelCalls != 1 || total[0].CostPUSD != 100 ||
			total[0].Transcription != wantTx {
			t.Errorf("the total: %+v", total)
		}
		agents := report(store.CostByAgent)
		if len(agents) != 2 || agents[0].Key != "agt_y" || agents[0].Transcription.Calls != 0 || agents[1].Key != store.CostKeyTranscription ||
			agents[1].AgentID != "" || agents[1].TenantID != "" || agents[1].ModelCalls != 0 || agents[1].Transcription != wantTx {
			t.Errorf("by agent: %+v", agents)
		}
		tenants := report(store.CostByTenant)
		if len(tenants) != 2 || tenants[0].Key != store.CostKeySite || tenants[0].TenantID != "" || tenants[0].Transcription != wantTx ||
			tenants[1].Key != "ten_yuki" || tenants[1].TenantID != "ten_yuki" {
			t.Errorf("by tenant: %+v", tenants)
		}
		if ks := report(store.CostByKeySource); len(ks) != 1 || ks[0].Key != "school" || ks[0].ModelCalls != 1 || ks[0].Transcription.Calls != 2 {
			t.Errorf("by key source: %+v", ks)
		}
		if models := report(store.CostByModel); len(models) != 2 || models[0].Key != "school/gemini/gemini-flash-lite" ||
			models[0].Transcription.Calls != 2 || models[0].ModelCalls != 0 || models[1].Key != "school/openai/gpt-test" {
			t.Errorf("by model: %+v", models)
		}

		for _, bad := range []store.LLMCall{
			func() store.LLMCall { c := tx("t3", 1, ""); c.AgentID = "agt_y"; return c }(),
			func() store.LLMCall { c := tx("t3", 1, ""); c.TenantID = "ten_yuki"; return c }(),
			func() store.LLMCall { c := tx("t3", 1, ""); c.CourseID = "c1"; return c }(),
			func() store.LLMCall { c := tx("t3", 1, ""); c.Kind = "ocr"; return c }(),
			func() store.LLMCall { c := call("c3", day, yuki, 1); c.AgentID = ""; return c }(),
		} {
			if err := s.RecordLLMCall(ctx, bad); err == nil {
				t.Errorf("RecordLLMCall(%+v) was taken", bad)
			}
		}
	})
}

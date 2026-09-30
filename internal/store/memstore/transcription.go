package memstore

import (
	"cmp"
	"context"
	"fmt"
	"slices"
	"time"

	"github.com/AIShie-Education/AIShie-Agent-Runtime/internal/store"
)

// transcription is what the transcriber keeps: its credential, nil for
// none, and its jobs, by id, with the last place given.
type transcription struct {
	cred    *store.TranscriptionCredential
	jobs    map[string]store.TranscriptionJob
	lastJob int64
}

func copyCredential(c store.TranscriptionCredential) *store.TranscriptionCredential {
	for _, t := range []**time.Time{&c.LastOKAt, &c.RejectedAt} {
		if *t != nil {
			v := **t
			*t = &v
		}
	}
	return &c
}

func copyJob(j store.TranscriptionJob) store.TranscriptionJob {
	if j.Pages != nil {
		n := *j.Pages
		j.Pages = &n
	}
	if j.CostPUSD != nil {
		n := *j.CostPUSD
		j.CostPUSD = &n
	}
	if j.FinishedAt != nil {
		t := keep(*j.FinishedAt)
		j.FinishedAt = &t
	}
	return j
}

// TranscriptionCredential is the credential, or store.ErrNotFound.
func (s *Store) TranscriptionCredential(_ context.Context) (*store.TranscriptionCredential, error) {
	s.mu.Lock()
	defer s.mu.Unlock()
	if s.tx.cred == nil {
		return nil, fmt.Errorf("the transcription credential: %w", store.ErrNotFound)
	}
	return copyCredential(*s.tx.cred), nil
}

// PutTranscriptionCredential keeps c with its token, in place of the one
// before, whose secret goes.
func (s *Store) PutTranscriptionCredential(_ context.Context, c store.TranscriptionCredential, token store.Secret) error {
	if err := store.CheckTranscriptionCredential(c, token); err != nil {
		return err
	}
	s.mu.Lock()
	defer s.mu.Unlock()
	if _, ok := s.secrets[token.ID]; ok {
		return fmt.Errorf("secret %s: %w", token.ID, store.ErrExists)
	}
	token.CreatedAt = s.orNow(token.CreatedAt)
	s.secrets[token.ID] = copySecret(token)
	if old := s.tx.cred; old != nil {
		delete(s.secrets, old.SecretID)
	}
	c.SetAt = s.orNow(c.SetAt)
	c.LastOKAt, c.RejectedAt, c.LastError = nil, nil, ""
	s.tx.cred = copyCredential(c)
	s.rev++
	return nil
}

// DeleteTranscriptionCredential forgets the credential and destroys its
// secret; none is nothing, and moves the revision on no more than
// pgstore's does.
func (s *Store) DeleteTranscriptionCredential(_ context.Context) error {
	s.mu.Lock()
	defer s.mu.Unlock()
	if s.tx.cred == nil {
		return nil
	}
	delete(s.secrets, s.tx.cred.SecretID)
	s.tx.cred = nil
	s.rev++
	return nil
}

// NoteTranscriptionCredential records what Core made of the credential
// secretID, if it is still the one kept; it moves nothing on.
func (s *Store) NoteTranscriptionCredential(_ context.Context, secretID string, ok bool, at time.Time, why string) error {
	s.mu.Lock()
	defer s.mu.Unlock()
	c := s.tx.cred
	if c == nil || c.SecretID != secretID {
		return fmt.Errorf("the transcription credential %s: %w", secretID, store.ErrNotFound)
	}
	at = s.orNow(at)
	if ok {
		c.LastOKAt, c.RejectedAt, c.LastError = &at, nil, ""
		return nil
	}
	if why == "" {
		why = "refused"
	}
	c.RejectedAt, c.LastError = &at, why
	return nil
}

// PutTranscriptionJob keeps j in place of the job of its id, which keeps
// its place; a new one takes the next.
func (s *Store) PutTranscriptionJob(_ context.Context, j store.TranscriptionJob) (*store.TranscriptionJob, error) {
	if err := store.CheckTranscriptionJob(j); err != nil {
		return nil, err
	}
	s.mu.Lock()
	defer s.mu.Unlock()
	if old, ok := s.tx.jobs[j.ID]; ok {
		j.Seq = old.Seq
	} else {
		s.tx.lastJob++
		j.Seq = s.tx.lastJob
	}
	j.StartedAt, j.HeartbeatAt = s.orNow(j.StartedAt), s.orNow(j.HeartbeatAt)
	j = copyJob(j)
	s.tx.jobs[j.ID] = j
	out := copyJob(j)
	return &out, nil
}

// TranscriptionJobs lists the jobs as q says, newest first.
func (s *Store) TranscriptionJobs(_ context.Context, q store.JobQuery) ([]store.TranscriptionJob, error) {
	if err := store.CheckJobQuery(q); err != nil {
		return nil, err
	}
	s.mu.Lock()
	defer s.mu.Unlock()
	var out []store.TranscriptionJob
	for _, j := range s.tx.jobs {
		if (q.Status == "" || j.Status == q.Status) && (q.Before == 0 || j.Seq < q.Before) {
			out = append(out, copyJob(j))
		}
	}
	slices.SortFunc(out, func(x, y store.TranscriptionJob) int { return cmp.Compare(y.Seq, x.Seq) })
	if len(out) > q.Limit {
		out = out[:q.Limit]
	}
	return out, nil
}

// TranscriptionDay sums the jobs begun since since.
func (s *Store) TranscriptionDay(_ context.Context, since time.Time) (store.TranscriptionDay, error) {
	s.mu.Lock()
	defer s.mu.Unlock()
	var d store.TranscriptionDay
	for _, j := range s.tx.jobs {
		if j.StartedAt.Before(since) {
			continue
		}
		d.Pages += j.PagesSent
		switch j.Status {
		case store.JobDone:
			d.Documents++
		case store.JobFailed:
			d.Failed++
		case store.JobSkipped:
			d.Skipped++
		}
		if j.CostPUSD != nil {
			d.CostPUSD += *j.CostPUSD
		}
	}
	return d, nil
}

// InterruptTranscriptionJobs ends the jobs still working whose claim was
// last held before before.
func (s *Store) InterruptTranscriptionJobs(_ context.Context, before, at time.Time) (int64, error) {
	s.mu.Lock()
	defer s.mu.Unlock()
	at = s.orNow(at)
	var n int64
	for id, j := range s.tx.jobs {
		if j.Status == store.JobWorking && j.HeartbeatAt.Before(before) {
			j.Status, j.Reason, j.FinishedAt = store.JobDropped, store.ReasonInterrupted, &at
			s.tx.jobs[id] = copyJob(j)
			n++
		}
	}
	return n, nil
}

// PruneTranscriptionJobs destroys the jobs begun before before.
func (s *Store) PruneTranscriptionJobs(_ context.Context, before time.Time) (int64, error) {
	s.mu.Lock()
	defer s.mu.Unlock()
	var n int64
	for id, j := range s.tx.jobs {
		if j.StartedAt.Before(before) {
			delete(s.tx.jobs, id)
			n++
		}
	}
	return n, nil
}

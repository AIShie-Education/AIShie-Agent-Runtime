package worker

import "sync"

// scheduler bounds one agent's answers in progress (§7.4): at most max at
// once, at most a course's own limit in each course, and never one
// conversation twice at once in this process. It never queues: a row that
// finds no slot waits for the next poll, which finds it again, longest
// waiting first. It is safe for concurrent use.
type scheduler struct {
	max int

	mu        sync.Mutex
	running   int
	perCourse map[string]int
	convs     map[string]bool
}

func newScheduler(max int) *scheduler {
	return &scheduler{max: max, perCourse: map[string]int{}, convs: map[string]bool{}}
}

// tryStart takes a slot for answering conversation conv in course, whose
// own limit is perCourse, and reports whether it got one.
func (s *scheduler) tryStart(course, conv string, perCourse int) bool {
	s.mu.Lock()
	defer s.mu.Unlock()
	if s.convs[conv] || s.running >= s.max || s.perCourse[course] >= perCourse {
		return false
	}
	s.running++
	s.perCourse[course]++
	s.convs[conv] = true
	return true
}

// done gives back the slot tryStart took.
func (s *scheduler) done(course, conv string) {
	s.mu.Lock()
	defer s.mu.Unlock()
	delete(s.convs, conv)
	s.running--
	if s.perCourse[course]--; s.perCourse[course] <= 0 {
		delete(s.perCourse, course)
	}
}

// reserve marks conv as being worked on by something that is not an
// answer (an attempt sent again at a seat's start), so that no answer
// starts on it meanwhile. It takes no slot.
func (s *scheduler) reserve(conv string) bool {
	s.mu.Lock()
	defer s.mu.Unlock()
	if s.convs[conv] {
		return false
	}
	s.convs[conv] = true
	return true
}

// release ends a reserve.
func (s *scheduler) release(conv string) {
	s.mu.Lock()
	defer s.mu.Unlock()
	delete(s.convs, conv)
}

// busy is how many answers are in progress.
func (s *scheduler) busy() int {
	s.mu.Lock()
	defer s.mu.Unlock()
	return s.running
}

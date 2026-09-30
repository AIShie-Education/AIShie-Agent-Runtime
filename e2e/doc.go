// Package e2e is the runtime's end-to-end tests against a real AIshie
// Core (Core's docs/agent-runtime.md §8.2 item 3, and M1's "done when"): a
// student's own agent and a course tutor answer, the moved-on, duplicate and
// denied paths hold, proposals are followed, an idle agent long-polling its
// inbox notices a question within a second, a question withdrawn stops the
// answer being written to it, the transcriber gives the course's files
// their text versions in Core (alone, after the others: Core's queue is
// the whole site's), and no token reaches a log.
//
// Each test seats its own people and agents through Core's REST API, as
// root and then as those people, each signed in with a password of their
// own (only agents are given API tokens), and runs the runtime in-process
// against that Core: a worker.Supervisor on memstore, its model the
// scripted OpenAI Chat server of internal/llm/fakellm behind the real
// openai_chat adapter. What Core shows its people is what the tests check.
// The binary is built and run against the same Core too, and a hosted agent
// is connected to a runtime whose registry is in PostgreSQL
// (TEST_DATABASE_URL, a scratch database of its own).
//
// The tests need a Core of their own, named by E2E_CORE_URL and
// E2E_ROOT_TOKEN (root's signed-in session, not an API token), which
// scripts/ci-core.sh start writes and scripts/e2e.sh (make e2e) sets up
// whole. Without them they skip, or fail when CI is true, so that CI's end
// to end never passes by testing nothing.
package e2e

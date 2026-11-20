package server

import (
	"encoding/json"
	"log"
	"strings"
	"time"

	podpkg "github.com/sandrpod/sandrpod/pkg/sandpod"
	"github.com/sandrpod/sandrpod/pkg/tunnel"
)

// Delivery is the step between "the request was accepted" and "the worker has
// been told to create the container". A process can die in the middle of it, and
// until this file existed that was silent data loss: the sandbox record and the
// job existed, and nothing knew the intent had never been delivered.
//
// The fix is the transactional-outbox pattern, using the database that is already
// there instead of a message broker. Three pieces:
//
//	create      writes the payload onto the job and commits it *before* delivery
//	deliver     pushes it down a tunnel (locally, or on the peer that holds it)
//	resume      re-delivers jobs whose delivery never completed, from any instance
//
// Delivery is at-least-once, so the worker side must tolerate a repeat. It does:
// a pode names the container after the sandbox, so a second create for the same
// name lands on the same container instead of a second one.
// resumeMaxAge bounds how old a pending create may be and still be delivered.
// Twice the 20-minute create budget: past that the job is a leftover, and
// re-delivering it resurrects a sandbox somebody deleted.
const resumeMaxAge = 40 * time.Minute

// jobRoutingKey is the record key for a job. Old rows predate the split and
// carried the name in SandboxName; new ones carry an opaque routing id.
func jobRoutingKey(j *podpkg.Job) string {
	if j.RoutingID != "" {
		return j.RoutingID
	}
	return j.SandboxName
}

var (
	// ResumeStaleAfter is how long a job may sit PENDING before another attempt
	// is made. Long enough that the in-flight delivery (2 min budget) usually
	// wins the race, short enough that a lost create recovers while a human is
	// still watching. `SANDRPOD_RESUME_STALE_AFTER` overrides it: the
	// multi-instance check plants a lost delivery and has to watch it be picked
	// up, and at the production cadence that wait is a race against CI timing
	// rather than a test of the resumer.
	ResumeStaleAfter = envDurationOr("SANDRPOD_RESUME_STALE_AFTER", 45*time.Second)
	// ResumeInterval is how often the resumer looks.
	// `SANDRPOD_RESUME_INTERVAL` overrides it.
	ResumeInterval = envDurationOr("SANDRPOD_RESUME_INTERVAL", 20*time.Second)
)

// ResumeMaxAttempts caps retries so a permanently broken request does not retry
// forever. Past it the job fails with the last reason.
const ResumeMaxAttempts = 5

// resumeDeps is what the resumer needs. Passed explicitly so the decision logic
// is testable without a database.
type resumeDeps struct {
	jobs      podpkg.JobRepository
	sandboxes podpkg.SandboxRepository
	tunnels   *tunnel.TunnelStore
	cfg       Config
	owners    podpkg.TunnelOwnerRepository
	deliver   func(*podpkg.Job, *podpkg.CreateSandboxRequest) error
	now       func() time.Time
}

// resumeOnce performs a single pass. It returns the number of jobs it delivered.
func resumeOnce(d resumeDeps) int {
	if d.jobs == nil || d.deliver == nil {
		return 0
	}
	now := time.Now()
	if d.now != nil {
		now = d.now()
	}
	pending := d.jobs.ListJobs()
	delivered := 0
	for _, job := range pending {
		if job == nil || job.Type != podpkg.JobTypeCreateSandbox {
			continue
		}
		// Only a job whose delivery never finished is ours. ListJobs returns every
		// row, and without this the resumer re-delivered *completed* creates every
		// minute: on a worker that was "Creating sandbox: smoke6" once a minute,
		// each pass making another container, until docker ran out of addresses.
		if job.Status != podpkg.JobStatusPending {
			continue
		}
		if now.Sub(job.UpdatedAt) < ResumeStaleAfter {
			continue // still within the in-flight budget
		}
		var req podpkg.CreateSandboxRequest
		if len(job.Request) == 0 {
			// Nothing to resend: this job was written by a process that died before
			// recording the intent. Fail the job *and* the record — a job marked
			// failed beside a sandbox that still says PENDING is the worst of both,
			// because the only thing the customer can see is the one that lies.
			_ = d.jobs.UpdateJob(job.ID, func(j *podpkg.Job) {
				j.Status = podpkg.JobStatusFailed
				j.ErrorMessage = "delivery payload was not recorded; cannot resume"
			})
			_ = d.sandboxes.Update(jobRoutingKey(job), func(sb *podpkg.SandboxInfo) {
				if sb.State == podpkg.StatePending {
					sb.State = podpkg.StateError
				}
			})
			continue
		}
		if err := json.Unmarshal(job.Request, &req); err != nil {
			_ = d.jobs.UpdateJob(job.ID, func(j *podpkg.Job) {
				j.Status = podpkg.JobStatusFailed
				j.ErrorMessage = "delivery payload is corrupt: " + err.Error()
			})
			continue
		}
		// A job older than the delivery window is archaeology, not a delivery in
		// flight. The create budget is 20 minutes; past twice that, the sandbox it
		// names was almost certainly deleted long ago and its record with it — and
		// re-delivering one of those recreates the sandbox, the network and the
		// container, once per pass, forever.
		if !job.CreatedAt.IsZero() && now.Sub(job.CreatedAt) > resumeMaxAge {
			_ = d.jobs.UpdateJob(job.ID, func(j *podpkg.Job) {
				j.Status = podpkg.JobStatusFailed
				j.ErrorMessage = "delivery window passed; the create never landed and is not retried"
			})
			continue
		}

		// The record decides, and its absence is not a verdict.
		//
		// The create path writes the job first and the record second, so "no
		// record" is the normal state of a delivery that never got going — exactly
		// the case this pass exists for. Failing it turned a lost delivery into a
		// permanent failure and broke the multi-instance check, which plants
		// precisely that row.
		//
		// A deletion does not produce this state either: the delete path fails the
		// sandbox's pending jobs itself, so by the time a job could be re-delivered
		// it is no longer pending.
		//
		//   absent      never delivered → deliver
		//   RUNNING     up already: the completion write was what got lost
		//   terminal    ERROR/STOPPED/TERMINATED: retrying cannot fix it
		sb, exists := d.sandboxes.Get(jobRoutingKey(job))
		if !exists {
			// A record that the create path never wrote (its delivery timed out
			// while a 2 GB image was still pulling). The job payload is the only
			// description left, so take the fields a client reads back from it —
			// without them the console cannot tell it is a desktop sandbox and its
			// row shows no image at all.
			sb = &podpkg.SandboxInfo{Name: jobRoutingKey(job), DisplayName: job.SandboxName, State: podpkg.StatePending}
			var req podpkg.CreateSandboxRequest
			if len(job.Request) > 0 && json.Unmarshal(job.Request, &req) == nil {
				sb.ImageID = req.ImageID
				sb.Desktop = req.Desktop || strings.Contains(strings.ToLower(req.ImageID), "desktop")
				sb.CPUCores = req.CPUCores
				sb.MemoryMB = req.MemoryMB
				sb.StorageMB = req.StorageMB
				sb.TTLSeconds = req.TTLSeconds
				sb.Region = req.Region
				sb.ProviderType = req.ProviderType
			}
			exists = true
		}
		switch sb.State {
		case podpkg.StateRunning:
			_ = d.jobs.UpdateJob(job.ID, func(j *podpkg.Job) {
				j.Status = podpkg.JobStatusCompleted
				j.ErrorMessage = ""
			})
			continue
		case podpkg.StateError, podpkg.StateStopped, podpkg.StateTerminated:
			_ = d.jobs.UpdateJob(job.ID, func(j *podpkg.Job) {
				j.Status = podpkg.JobStatusFailed
				j.ErrorMessage = "sandbox is " + string(sb.State) + "; nothing to deliver"
			})
			continue
		}
		// Claim before delivering. Every instance runs this pass over the same
		// rows, and without a claim they would all send the same create — the
		// promise is at-least-once, not "once per node".
		claimed, err := claimJob(d.jobs, job, now)
		if err != nil || !claimed {
			continue // someone else got there first, or is ahead of us
		}
		if err := d.deliver(job, &req); err != nil {
			log.Printf("resume: job %s (%s): %v", job.ID, job.SandboxName, err)
			continue
		}
		delivered++
		// The delivery is what the job was for, so it is finished. Leaving it
		// PENDING re-delivers it on the next pass — and a create is not idempotent
		// at the container level, so each pass made another sandbox container:
		// observed as "Creating sandbox: smoke6" once a minute, on and on.
		_ = d.jobs.UpdateJob(job.ID, func(j *podpkg.Job) {
			j.Status = podpkg.JobStatusCompleted
			j.ErrorMessage = ""
		})
		log.Printf("resume: delivered job %s (%s) on attempt after %s",
			job.ID, job.SandboxName, now.Sub(job.UpdatedAt).Truncate(time.Second))
	}
	return delivered
}

// claimJob is the compare-and-swap, done on the row's own timestamp spelling when
// the store has one. Re-formatting a timestamp is not the identity function: the
// same instant can be "…40.813130Z" (Postgres to_char) or "…40.81313Z" (Go), and
// a claim that compares the wrong spelling never matches, so the job sits PENDING
// forever with nothing in the log.
func claimJob(jobs podpkg.JobRepository, job *podpkg.Job, now time.Time) (bool, error) {
	if job.UpdatedAtRaw != "" {
		if raw, ok := jobs.(podpkg.RawTimeClaimer); ok {
			return raw.ClaimJobRaw(job.ID, job.UpdatedAtRaw, now)
		}
	}
	return jobs.ClaimJob(job.ID, job.UpdatedAt, now)
}

// startResumer runs the pass on a timer for the life of the process.
func startResumer(d resumeDeps) {
	go func() {
		ticker := time.NewTicker(ResumeInterval)
		defer ticker.Stop()
		for range ticker.C {
			resumeOnce(d)
		}
	}()
}

// deliverCreate resumes one create: either through a tunnel held here, or by
// asking the peer that holds it.
func deliverCreate(d resumeDeps, job *podpkg.Job, req *podpkg.CreateSandboxRequest) error {
	if t, ok := d.tunnels.Get(job.PoderID); ok {
		_, _, err := pushCreateToPoder(t, req, job.ID, job.PoderID, d.sandboxes, d.jobs,
			func(msg string) {
				_ = d.jobs.UpdateJob(job.ID, func(j *podpkg.Job) {
					j.Status = podpkg.JobStatusFailed
					j.ErrorMessage = msg
				})
			})
		return err
	}
	if peer, ok := wantsPeerDelivery(d.cfg, d.owners, job.PoderID); ok {
		_, err := deliverCreateToPeer(peer, d.cfg.Token, req, job.ID, job.Owner, job.PoderID)
		return err
	}
	// No tunnel anywhere: the worker is gone. Fail with a reason rather than
	// retrying a target that no longer exists.
	_ = d.jobs.UpdateJob(job.ID, func(j *podpkg.Job) {
		j.Status = podpkg.JobStatusFailed
		j.ErrorMessage = "worker " + job.PoderID + " is not connected to any control-plane instance"
	})
	return errNoWorker
}

type resumeError string

func (e resumeError) Error() string { return string(e) }

const errNoWorker = resumeError("worker not connected")

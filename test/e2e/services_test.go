//go:build e2e

package e2e

import (
	"path/filepath"
	"strings"
	"testing"
	"time"
)

type revisionRow struct {
	Name, Ready, Image string
	Percent            int64
	Tags               []string
}

func (s *suite) revisions(t *testing.T, dir string) []revisionRow {
	t.Helper()
	r := s.code(t, s.cli(t, dir, "revisions", s.service, "--format", "json"), 0)
	rows := decode[[]revisionRow](s, t, []byte(r.stdout))
	if len(rows) == 0 {
		s.fatal(t, "expected revisions")
	}
	return rows
}
func (s *suite) cleanDiff(t *testing.T, dir string) {
	t.Helper()
	r := s.code(t, s.cli(t, dir, "diff", "--exit-code"), 0)
	s.empty(t, r.stdout)
}
func (s *suite) services(t *testing.T) {
	dir := s.dir(t, "current")
	// Keep an ambient gcloud target to prove clrnd only accepts its explicit target.
	r := s.command(t, s.ctx, dir, map[string]string{"CLOUDSDK_CORE_PROJECT": s.c.project, "CLOUDSDK_RUN_REGION": s.c.region}, s.binary, "status", s.service)
	s.fails(t, r)
	s.contains(t, r.stderr, "does not take the target from the environment")
	s.config(t, dir)
	manifest := filepath.Join(dir, "manifest.yaml")
	s.write(t, manifest, serviceManifest(s.service, s.c.image))
	r = s.code(t, s.cli(t, dir, "deploy", s.service, manifest, "--auto-approve"), 0)
	s.contains(t, r.stderr, "project: "+s.c.project)
	s.contains(t, r.stderr, s.c.region)
	s.ready(t)
	r = s.code(t, s.cli(t, dir, "status", s.service), 0)
	for _, want := range []string{"Ready:           True", "URL:             https://", "100%"} {
		s.contains(t, r.stdout, want)
	}
	r = s.code(t, s.cli(t, dir, "status", s.service, "--format", "json"), 0)
	status := decode[struct {
		Service    string
		Conditions []map[string]any
	}](s, t, []byte(r.stdout))
	if status.Service != s.service || len(status.Conditions) == 0 {
		s.fatal(t, "status JSON lacks service or conditions")
	}
	r = s.code(t, s.cli(t, dir, "wait", s.service, "--timeout", "120s"), 0)
	s.empty(t, r.stdout)
	s.contains(t, r.stderr, "Ready=True")
	r = s.code(t, s.cli(t, dir, "revisions", s.service), 0)
	for _, want := range []string{"REVISION", "100%", s.c.image} {
		s.contains(t, r.stdout, want)
	}
	s.revisions(t, dir)
	r = s.code(t, s.cli(t, dir, "diff", s.service, manifest, "--no-server-defaults"), 0)
	for _, want := range []string{"containerConcurrency", "startupProbe", "latestRevision"} {
		s.contains(t, r.stdout, want)
	}
	for _, unwanted := range []string{"cloud.googleapis.com/location", "serving.knative.dev/creator"} {
		s.lacks(t, r.stdout, unwanted)
	}
	r = s.code(t, s.cli(t, dir, "diff", s.service, manifest), 0)
	s.empty(t, r.stdout)

	t.Log("Scaffold a live definition with a pinned revision, then change it")
	s.pinLive(t, "pinned")
	pinned := s.serviceState(t).Spec.Template.Metadata.Name
	if pinned == "" {
		s.fatal(t, "gcloud did not pin the live revision")
	}
	dir = s.dir(t, "current-init")
	s.code(t, s.cli(t, dir, "init", s.service, "--project", s.c.project, "--region", s.c.region), 0)
	manifest = filepath.Join(dir, "manifest.yaml")
	text := string(s.read(t, manifest))
	s.lacks(t, text, pinned)
	s.lacks(t, text, "metadata: {}")
	s.cleanDiff(t, dir)
	s.edit(t, manifest, func(m map[string]any) { setEnv(m, "second") })
	s.code(t, s.cli(t, dir, "deploy", "--auto-approve"), 0)
	s.ready(t)
	before := s.serviceState(t).Status.LatestReadyRevisionName
	s.code(t, s.cli(t, dir, "refresh", s.service, "--auto-approve", "--timeout", "120s"), 0)
	s.ready(t)
	after := s.serviceState(t).Status.LatestReadyRevisionName
	if after == "" || after == before {
		s.fatal(t, "refresh did not create a revision")
	}
	s.cleanDiff(t, dir)

	rows := s.revisions(t, dir)
	expected := ""
	servingIndex := -1
	// The newest revision with traffic is the rollback baseline, including a canary.
	for i, v := range rows {
		if v.Percent > 0 {
			servingIndex = i
			break
		}
	}
	if servingIndex >= 0 {
		for _, v := range rows[servingIndex+1:] {
			if v.Ready == "True" {
				expected = v.Name
				break
			}
		}
	}
	if expected == "" {
		s.fatal(t, "no older ready rollback target")
	}
	s.code(t, s.cli(t, dir, "rollback", s.service, "--auto-approve", "--timeout", "120s"), 0)
	s.ready(t)
	s.eventually(t, time.Minute, func() bool { return s.serving(t) == expected })
	// The rollback leaves the latest created revision serving nothing, but Cloud Run refuses to
	// delete it, so a prune must not pick it.
	latestCreated := s.serviceState(t).Status.LatestCreatedRevisionName
	r = s.code(t, s.cli(t, dir, "revisions", s.service, "--prune", "--keep", "0", "--dry-run", "--format", "json"), 0)
	for _, v := range decode[[]revisionRow](s, t, []byte(r.stdout)) {
		if v.Name == latestCreated || v.Name == expected {
			s.fatal(t, "prune after a rollback picked %s", v.Name)
		}
	}
	currentTemplate := s.serviceState(t).Spec.Template.Metadata.Name
	if currentTemplate == "" {
		s.fatal(t, "refresh should have pinned a template revision")
	}
	suffix := strings.TrimPrefix(currentTemplate, s.service+"-")
	r = s.fails(t, s.cli(t, dir, "refresh", s.service, "--revision-suffix", suffix, "--auto-approve"))
	s.contains(t, r.stderr, "already the current template revision")
	r = s.fails(t, s.cli(t, dir, "refresh", s.service, "--auto-approve"))
	s.contains(t, r.stderr, "receives no traffic")
	s.edit(t, manifest, func(m map[string]any) { setEnv(m, "after-rollback") })
	s.code(t, s.cli(t, dir, "deploy", "--auto-approve", "--timeout", "120s"), 0)
	latest := s.serviceState(t).Status.LatestReadyRevisionName
	previous := ""
	for _, v := range s.revisions(t, dir) {
		if v.Name != latest && v.Ready == "True" {
			previous = v.Name
			break
		}
	}
	if previous == "" {
		s.fatal(t, "no older ready canary revision")
	}
	count := s.revisionCount(t)
	s.code(t, s.cli(t, dir, "traffic", s.service, "--to", previous, "--percent", "20", "--auto-approve", "--timeout", "120s"), 0)
	s.ready(t)
	s.percent(t, previous, 20)
	s.percent(t, latest, 80)
	if s.revisionCount(t) != count {
		s.fatal(t, "traffic created a revision")
	}
	s.code(t, s.cli(t, dir, "traffic", s.service, "--to-latest", "--auto-approve", "--timeout", "120s"), 0)
	s.ready(t)
	s.eventually(t, time.Minute, func() bool { return s.serving(t) == latest })
	s.percent(t, latest, 100)
	followsLatest := false
	for _, v := range s.serviceState(t).Spec.Traffic {
		followsLatest = followsLatest || v.LatestRevision
	}
	if !followsLatest {
		s.fatal(t, "--to-latest left traffic pinned")
	}

	t.Log("Deploy without traffic, then move traffic to the new revision")
	servingBefore := s.serving(t)
	s.edit(t, manifest, func(m map[string]any) { setEnv(m, "no-traffic") })
	s.code(t, s.cli(t, dir, "deploy", "--no-traffic", "--auto-approve", "--timeout", "120s"), 0)
	newRevision := s.serviceState(t).Status.LatestCreatedRevisionName
	if newRevision == "" || newRevision == servingBefore {
		s.fatal(t, "--no-traffic created no new revision")
	}
	s.percent(t, newRevision, 0)
	if s.serving(t) != servingBefore {
		s.fatal(t, "--no-traffic moved traffic")
	}
	s.code(t, s.cli(t, dir, "traffic", s.service, "--to-latest", "--auto-approve", "--timeout", "120s"), 0)
	s.ready(t)
	s.eventually(t, time.Minute, func() bool { return s.serving(t) == newRevision })
	r = s.code(t, s.cli(t, dir, "verify"), 0)
	s.empty(t, r.stdout)
	s.lacks(t, r.stderr, "warning:")
	r = s.code(t, s.cli(t, dir, "verify", "--format", "json"), 0)
	verified := decode[struct {
		OK      bool
		Service string
		Missing []string
	}](s, t, []byte(r.stdout))
	if !verified.OK || verified.Service != s.service || len(verified.Missing) != 0 {
		s.fatal(t, "unexpected verify JSON: %s", r.stdout)
	}

	t.Log("A pinned revision refuses template changes, including a retry")
	pinnedFile := filepath.Join(dir, "pinned.yaml")
	s.write(t, pinnedFile, s.read(t, manifest))
	revision := s.serviceState(t).Status.LatestReadyRevisionName
	s.edit(t, pinnedFile, func(m map[string]any) { pinRevision(m, revision) })
	s.contains(t, string(s.read(t, pinnedFile)), revision)
	r = s.code(t, s.cli(t, dir, "verify", s.service, pinnedFile, "--local-only"), 0)
	s.contains(t, r.stderr, "warning:")
	s.edit(t, pinnedFile, func(m map[string]any) { setEnv(m, "third") })
	r = s.fails(t, s.cli(t, dir, "deploy", s.service, pinnedFile, "--auto-approve", "--timeout", "120s"))
	s.contains(t, r.stderr, "warning:")
	s.fails(t, s.cli(t, dir, "deploy", s.service, pinnedFile, "--auto-approve", "--timeout", "60s"))

	s.images(t, dir)
	s.render(t)
	s.prune(t, dir)
	if s.c.oldRef == "" {
		r = s.code(t, s.cli(t, dir, "delete", s.service, "--dry-run"), 0)
		s.contains(t, r.stderr, "About to delete:")
		s.serviceState(t)
		s.code(t, s.cli(t, dir, "delete", s.service, "--auto-approve", "--timeout", "120s"), 0)
		s.absent(t, "service")
	}
}

func (s *suite) images(t *testing.T, dir string) {
	t.Helper()
	imageDir := s.dir(t, "current-image")
	good := filepath.Join(imageDir, "manifest.yaml")
	s.write(t, good, serviceManifest(s.service, s.c.image))
	r := s.code(t, s.cli(t, dir, "verify", s.service, good), 0)
	s.lacks(t, r.stderr, "warning:")
	bad := filepath.Join(imageDir, "bad-tag.yaml")
	s.write(t, bad, serviceManifest(s.service, s.c.image+":clrnd-e2e-no-such-tag"))
	r = s.fails(t, s.cli(t, dir, "verify", s.service, bad))
	s.contains(t, r.stderr, "does not exist")
	gcr := filepath.Join(imageDir, "gcr.yaml")
	s.write(t, gcr, serviceManifest(s.service, "gcr.io/clrnd-e2e-no-such-project/no-such-image:v1"))
	r = s.code(t, s.cli(t, dir, "verify", s.service, gcr), 0)
	s.lacks(t, r.stderr, "warning:")
	s.code(t, s.cli(t, dir, "verify", s.service, bad, "--image", s.c.image), 0)
	s.code(t, s.cli(t, dir, "deploy", s.service, bad, "--image", s.c.image, "--auto-approve", "--timeout", "120s"), 0)
	if s.serviceState(t).Spec.Template.Spec.Containers[0].Image != s.c.image {
		s.fatal(t, "image override was not applied")
	}
	r = s.fails(t, s.cli(t, dir, "deploy", s.service, bad, "--image", "sidecar="+s.c.image, "--auto-approve"))
	s.contains(t, r.stderr, "does not define")
}
func (s *suite) prune(t *testing.T, dir string) {
	t.Helper()
	before := s.revisionCount(t)
	serving := s.serving(t)
	s.code(t, s.cli(t, dir, "revisions", s.service, "--prune", "--keep", "1", "--dry-run"), 0)
	if s.revisionCount(t) != before {
		s.fatal(t, "dry run deleted revisions")
	}
	// The JSON carries no spec.traffic marker, so read what prune protects from the live spec.
	svc := s.serviceState(t)
	protected := map[string]bool{}
	for _, v := range svc.Spec.Traffic {
		protected[v.RevisionName] = true
		if v.LatestRevision {
			protected[svc.Status.LatestCreatedRevisionName] = true
			protected[svc.Status.LatestReadyRevisionName] = true
		}
	}
	target := ""
	for _, v := range s.revisions(t, dir)[1:] {
		if v.Percent == 0 && len(v.Tags) == 0 && !protected[v.Name] {
			target = v.Name
			break
		}
	}
	if target == "" {
		s.fatal(t, "no prunable revision")
	}
	s.code(t, s.cli(t, dir, "revisions", s.service, "--prune", "--keep", "1", "--auto-approve"), 0)
	s.eventually(t, time.Minute, func() bool { return !s.revisionExists(t, target) })
	s.code(t, s.cli(t, dir, "revisions", s.service, "--prune", "--keep", "0", "--auto-approve"), 0)
	if !s.revisionExists(t, serving) {
		s.fatal(t, "prune deleted the serving revision")
	}
	if s.serving(t) != serving {
		s.fatal(t, "prune changed traffic")
	}
	s.eventually(t, time.Minute, func() bool { return s.revisionCount(t) < before })
	s.code(t, s.cli(t, dir, "status"), 0)
}

func (s *suite) comparison(t *testing.T) {
	dir := s.dir(t, "old-init")
	// gcloud creates the precondition even when ONLY=old skipped the service scenario.
	s.pinLive(t, "phase2")
	pinned := s.serviceState(t).Spec.Template.Metadata.Name
	// Releases before the explicit-target change take the target from these variables only, and
	// a missing target would read below as the old binary refusing the change.
	env := map[string]string{"CLOUDSDK_CORE_PROJECT": s.c.project, "CLOUDSDK_RUN_REGION": s.c.region}
	r := s.run(t, s.oldBinary, dir, env, "init", s.service, "--project", s.c.project, "--region", s.c.region)
	s.code(t, r, 0)
	manifest := filepath.Join(dir, "manifest.yaml")
	if pinned != "" && strings.Contains(string(s.read(t, manifest)), pinned) {
		s.log(t, "comparison binary pins the live revision name in its scaffold")
	} else {
		s.log(t, "comparison binary does not pin the revision name")
	}
	s.edit(t, manifest, func(m map[string]any) { setEnv(m, "phase2") })
	r = s.run(t, s.oldBinary, dir, env, "deploy", "--auto-approve")
	// Historical binaries may return success before an asynchronous failure. This phase is
	// observational: record that outcome, rather than assuming a particular old bug exists.
	if r.code != 0 {
		s.log(t, "comparison binary refused the template change")
		return
	}
	timer := time.NewTimer(5 * time.Second)
	defer timer.Stop()
	select {
	case <-s.ctx.Done():
		s.fatal(t, "comparison interrupted")
	case <-timer.C:
	}
	for _, c := range s.serviceState(t).Status.Conditions {
		if c.Type == "Ready" {
			s.log(t, "comparison Ready="+c.Status+" "+c.Reason)
		}
	}
}

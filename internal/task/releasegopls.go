// Copyright 2020 The Go Authors. All rights reserved.
// Use of this source code is governed by a BSD-style
// license that can be found in the LICENSE file.

package task

import (
	"errors"
	"fmt"
	"slices"
	"strconv"
	"strings"
	"time"

	"github.com/google/go-github/v48/github"
	"golang.org/x/build/gerrit"
	"golang.org/x/build/internal/relui/groups"
	wf "golang.org/x/build/internal/workflow"
	"golang.org/x/mod/semver"
)

// ReleaseGoplsTasks provides workflow definitions and tasks for releasing gopls.
type ReleaseGoplsTasks struct {
	Github             GitHubClientInterface
	Gerrit             GerritClient
	CloudBuild         CloudBuildClient
	SendMail           func(MailHeader, MailContent) error
	AnnounceMailHeader MailHeader
	ApproveAction      func(*wf.TaskContext) error
}

// NewPrereleaseDefinition create a new workflow definition for gopls pre-release.
func (r *ReleaseGoplsTasks) NewPrereleaseDefinition() *wf.Definition {
	wd := wf.New(wf.ACL{Groups: []string{groups.ToolsTeam}})

	// TODO(hxjiang): provide potential release versions in the relui where the
	// coordinator can choose which version to release instead of manual input.
	version := wf.Param(wd, wf.ParamDef[string]{Name: "version"})
	reviewers := wf.Param(wd, reviewersParam)

	semv := wf.Task1(wd, "validating input version", r.isValidReleaseVersion, version)
	prerelease := wf.Task1(wd, "find the pre-release version", r.nextPrerelease, semv)
	approved := wf.Action2(wd, "wait for release coordinator approval", r.approveVersion, semv, prerelease)

	issue := wf.Task1(wd, "create release git issue", r.createReleaseIssue, semv, wf.After(approved))
	branchCreated := wf.Action1(wd, "creating new branch if minor release", r.createBranchIfMinor, semv, wf.After(issue))

	configChangeID := wf.Task3(wd, "updating branch's codereview.cfg", r.updateCodeReviewConfig, semv, reviewers, issue, wf.After(branchCreated))
	configCommit := wf.Task1(wd, "await config CL submission", clAwaiter{r.Gerrit}.awaitSubmission, configChangeID)

	dependencyChangeID := wf.Task4(wd, "updating gopls' x/tools dependency", r.updateXToolsDependency, semv, prerelease, reviewers, issue, wf.After(configCommit))
	dependencyCommit := wf.Task1(wd, "await gopls' x/tools dependency CL submission", clAwaiter{r.Gerrit}.awaitSubmission, dependencyChangeID)

	verified := wf.Action1(wd, "verify installing latest gopls using release branch dependency commit", r.verifyGoplsInstallation, dependencyCommit)
	prereleaseVersion := wf.Task3(wd, "tag pre-release", r.tagPrerelease, semv, dependencyCommit, prerelease, wf.After(verified))
	prereleaseVerified := wf.Action1(wd, "verify installing latest gopls using release branch pre-release version", r.verifyGoplsInstallation, prereleaseVersion)
	wf.Action4(wd, "mail announcement", r.mailAnnouncement, semv, prereleaseVersion, dependencyCommit, issue, wf.After(prereleaseVerified))

	wf.Output(wd, "version", prereleaseVersion)

	return wd
}

func (r *ReleaseGoplsTasks) approveVersion(ctx *wf.TaskContext, semv semversion, pre string) error {
	ctx.Printf("The next release candidate will be v%v.%v.%v-%s", semv.Major, semv.Minor, semv.Patch, pre)
	return r.ApproveAction(ctx)
}

// createReleaseIssue attempts to locate the release issue associated with the
// given milestone. If no such issue exists, a new one is created.
//
// Returns the ID of the release issue (either newly created or pre-existing).
// Returns error if the release milestone does not exist or is closed.
func (r *ReleaseGoplsTasks) createReleaseIssue(ctx *wf.TaskContext, semv semversion) (int64, error) {
	versionString := fmt.Sprintf("v%v.%v.%v", semv.Major, semv.Minor, semv.Patch)
	milestoneName := fmt.Sprintf("gopls/%s", versionString)
	// All milestones and issues resides under go repo.
	milestoneID, err := r.Github.FetchMilestone(ctx, "golang", "go", milestoneName, false)
	if err != nil {
		return 0, err
	}
	ctx.Printf("found release milestone %v", milestoneID)
	issues, err := r.Github.FetchMilestoneIssues(ctx, "golang", "go", milestoneID)
	if err != nil {
		return 0, err
	}

	title := fmt.Sprintf("x/tools/gopls: release version %s", versionString)
	for id := range issues {
		issue, _, err := r.Github.GetIssue(ctx, "golang", "go", id)
		if err != nil {
			return 0, err
		}
		if title == issue.GetTitle() {
			ctx.Printf("found existing releasing issue %v", id)
			return int64(id), nil
		}
	}

	content := fmt.Sprintf(`This issue tracks progress toward releasing gopls@%s

- [ ] create or update %s
- [ ] update go.mod/go.sum (remove x/tools replace, update x/tools version)
- [ ] tag gopls/%s-pre.1
- [ ] update Github milestone
- [ ] write release notes
- [ ] smoke test features
- [ ] tag gopls/%s
- [ ] (if vX.Y.0 release): update dependencies in master for the next release
`, versionString, goplsReleaseBranchName(semv), versionString, versionString)
	// TODO(hxjiang): accept a new parameter release coordinator.
	assignee := "h9jiang"
	issue, _, err := r.Github.CreateIssue(ctx, "golang", "go", &github.IssueRequest{
		Title:     &title,
		Body:      &content,
		Labels:    &[]string{"gopls", "Tools"},
		Assignee:  &assignee,
		Milestone: &milestoneID,
	})
	if err != nil {
		return 0, fmt.Errorf("failed to create release tracking issue for %q: %w", versionString, err)
	}
	ctx.Printf("created releasing issue %v", *issue.Number)
	return int64(*issue.Number), nil
}

// goplsReleaseBranchName returns the branch name for given input release version.
func goplsReleaseBranchName(semv semversion) string {
	return fmt.Sprintf("gopls-release-branch.%v.%v", semv.Major, semv.Minor)
}

// createBranchIfMinor create the release branch if the input version is a minor
// release.
// All patch releases under the same minor version share the same release branch.
func (r *ReleaseGoplsTasks) createBranchIfMinor(ctx *wf.TaskContext, semv semversion) error {
	branch := goplsReleaseBranchName(semv)

	// Require gopls release branch existence if this is a non-minor release.
	if semv.Patch != 0 {
		_, err := r.Gerrit.ReadBranchHead(ctx, "tools", branch)
		return err
	}

	// Return early if the branch already exist.
	// This scenario should only occur if the initial minor release flow failed
	// or was interrupted and subsequently re-triggered.
	if _, err := r.Gerrit.ReadBranchHead(ctx, "tools", branch); err == nil {
		return nil
	}

	// Create the release branch using the revision from the head of master branch.
	head, err := r.Gerrit.ReadBranchHead(ctx, "tools", "master")
	if err != nil {
		return err
	}

	ctx.Printf("Creating branch %s at revision %s.\n", branch, head)
	_, err = r.Gerrit.CreateBranch(ctx, "tools", branch, gerrit.BranchInput{Revision: head})
	return err
}

// openCL checks if an open CL with the given title exists in the specified
// branch.
//
// It returns an empty string if no such CL is found, otherwise it returns the
// CL's change ID.
func openCL(ctx *wf.TaskContext, gerrit GerritClient, branch, title string) (string, error) {
	// Query for an existing pending config CL, to avoid duplication.
	query := fmt.Sprintf(`message:%q status:open owner:gobot@golang.org repo:tools branch:%q -age:7d`, title, branch)
	changes, err := gerrit.QueryChanges(ctx, query)
	if err != nil {
		return "", err
	}
	if len(changes) == 0 {
		return "", nil
	}

	return changes[0].ChangeID, nil
}

// updateCodeReviewConfig checks if codereview.cfg has the desired configuration.
//
// It returns the change ID required to update the config if changes are needed,
// otherwise it returns an empty string indicating no update is necessary.
func (r *ReleaseGoplsTasks) updateCodeReviewConfig(ctx *wf.TaskContext, semv semversion, reviewers []string, issue int64) (string, error) {
	const configFile = "codereview.cfg"

	branch := goplsReleaseBranchName(semv)
	clTitle := fmt.Sprintf("all: update %s for %s", configFile, branch)

	openCL, err := openCL(ctx, r.Gerrit, branch, clTitle)
	if err != nil {
		return "", fmt.Errorf("failed to find the open CL of title %q in branch %q: %w", clTitle, branch, err)
	}
	if openCL != "" {
		ctx.Printf("not creating CL: found existing CL %s", openCL)
		return openCL, nil
	}

	head, err := r.Gerrit.ReadBranchHead(ctx, "tools", branch)
	if err != nil {
		return "", err
	}

	before, err := r.Gerrit.ReadFile(ctx, "tools", head, configFile)
	if err != nil && !errors.Is(err, gerrit.ErrResourceNotExist) {
		return "", err
	}
	const configFmt = `issuerepo: golang/go
branch: %s
parent-branch: master
`
	after := fmt.Sprintf(configFmt, branch)
	// Skip CL creation as config has not changed.
	if string(before) == after {
		return "", nil
	}

	changeInput := gerrit.ChangeInput{
		Project: "tools",
		Subject: fmt.Sprintf("%s\n\nThis is an automated CL which updates the %s.\n\nFor golang/go#%v", clTitle, configFile, issue),
		Branch:  branch,
	}

	files := map[string]string{
		configFile: string(after),
	}

	ctx.Printf("creating auto-submit change to %s under branch %q in x/tools repo.", configFile, branch)
	return r.Gerrit.CreateAutoSubmitChange(ctx, changeInput, reviewers, files)
}

// nextPrerelease inspects the tags in tools repo that match with the given
// version and find the next prerelease version.
func (r *ReleaseGoplsTasks) nextPrerelease(ctx *wf.TaskContext, semv semversion) (string, error) {
	cur, err := currentGoplsPrerelease(ctx, r.Gerrit, semv)
	if err != nil {
		return "", err
	}
	return fmt.Sprintf("pre.%v", cur+1), nil
}

// currentGoplsPrerelease inspects the tags in tools repo that match with the
// given version and find the latest pre-release version.
func currentGoplsPrerelease(ctx *wf.TaskContext, client GerritClient, semv semversion) (int, error) {
	tags, err := client.ListTags(ctx, "tools")
	if err != nil {
		return 0, fmt.Errorf("failed to list tags for tools repo: %w", err)
	}

	max := 0
	for _, tag := range tags {
		v, ok := strings.CutPrefix(tag, "gopls/")
		if !ok {
			continue
		}
		cur, ok := parseSemver(v)
		if !ok {
			continue
		}
		if cur.Major != semv.Major || cur.Minor != semv.Minor || cur.Patch != semv.Patch {
			continue
		}
		pre, err := cur.prereleaseVersion()
		if err != nil {
			continue
		}

		if pre > max {
			max = pre
		}
	}

	return max, nil
}

// updateXToolsDependency ensures gopls sub module have the correct x/tools
// version as dependency.
//
// It returns the change ID, or "" if the CL was not created.
func (r *ReleaseGoplsTasks) updateXToolsDependency(ctx *wf.TaskContext, semv semversion, pre string, reviewers []string, issue int64) (string, error) {
	if pre == "" {
		return "", fmt.Errorf("the input pre-release version should not be empty")
	}

	branch := goplsReleaseBranchName(semv)
	clTitle := fmt.Sprintf("gopls: update go.mod for v%v.%v.%v-%s", semv.Major, semv.Minor, semv.Patch, pre)
	openCL, err := openCL(ctx, r.Gerrit, branch, clTitle)
	if err != nil {
		return "", fmt.Errorf("failed to find the open CL of title %q in branch %q: %w", clTitle, branch, err)
	}
	if openCL != "" {
		ctx.Printf("not creating CL: found existing CL %s", openCL)
		return openCL, nil
	}

	head, err := r.Gerrit.ReadBranchHead(ctx, "tools", branch)
	if err != nil {
		return "", err
	}
	// TODO(hxjiang): Remove -compat flag once gopls no longer supports building
	// with older Go versions.
	script := fmt.Sprintf(`cd gopls
go mod edit -dropreplace=golang.org/x/tools
go get golang.org/x/tools@%s
go mod tidy -compat=1.19
`, head)

	changedFiles, err := executeAndMonitorChange(ctx, r.CloudBuild, branch, script, []string{"gopls/go.mod", "gopls/go.sum"})
	if err != nil {
		return "", err
	}

	// Skip CL creation as nothing changed.
	if len(changedFiles) == 0 {
		return "", nil
	}

	changeInput := gerrit.ChangeInput{
		Project: "tools",
		Branch:  branch,
		Subject: fmt.Sprintf("%s\n\nThis is an automated CL which updates the go.mod and go.sum.\n\nFor golang/go#%v", clTitle, issue),
	}

	ctx.Printf("creating auto-submit change under branch %q in x/tools repo.", branch)
	return r.Gerrit.CreateAutoSubmitChange(ctx, changeInput, reviewers, changedFiles)
}

// verifyGoplsInstallation installs the gopls with the provided version and run
// smoke test.
// The input version can be a commit, a branch name, a semantic version, see
// more detail https://go.dev/ref/mod#version-queries
func (r *ReleaseGoplsTasks) verifyGoplsInstallation(ctx *wf.TaskContext, version string) error {
	if version == "" {
		return fmt.Errorf("the input version should not be empty")
	}
	const scriptFmt = `go install golang.org/x/tools/gopls@%s &> install.log
$(go env GOPATH)/bin/gopls version &> version.log
echo -n "package main

func main () {
	const a = 2
	b := a
}" > main.go
$(go env GOPATH)/bin/gopls references -d main.go:4:8 &> smoke.log
`

	ctx.Printf("verify gopls with version %s\n", version)
	build, err := r.CloudBuild.RunScript(ctx, fmt.Sprintf(scriptFmt, version), "", []string{"install.log", "version.log", "smoke.log"})
	if err != nil {
		return err
	}

	outputs, err := buildToOutputs(ctx, r.CloudBuild, build)
	if err != nil {
		return err
	}
	ctx.Printf("verify gopls installation process:\n%s\n", outputs["install.log"])
	ctx.Printf("verify gopls version:\n%s\n", outputs["version.log"])
	ctx.Printf("verify gopls functionality with gopls references smoke test:\n%s\n", outputs["smoke.log"])
	return nil
}

// tagPrerelease applies gopls pre-release tags to the given commit.
// The input semversion provides Major, Minor, and Patch info.
// The input pre-release, generated by previous steps of the workflow, provides
// Pre-release info.
func (r *ReleaseGoplsTasks) tagPrerelease(ctx *wf.TaskContext, semv semversion, commit, pre string) (string, error) {
	if commit == "" {
		return "", fmt.Errorf("the input commit should not be empty")
	}
	if pre == "" {
		return "", fmt.Errorf("the input pre-release version should not be empty")
	}

	// Defensively guard against re-creating tags.
	ctx.DisableRetries()

	version := fmt.Sprintf("v%v.%v.%v-%s", semv.Major, semv.Minor, semv.Patch, pre)
	tag := fmt.Sprintf("gopls/%s", version)
	if err := r.Gerrit.Tag(ctx, "tools", tag, commit); err != nil {
		return "", err
	}

	ctx.Printf("tagged commit %s with tag %s", commit, tag)
	return version, nil
}

type goplsPrereleaseAnnouncement struct {
	Version string
	Branch  string
	Commit  string
	Issue   int64
}

func (r *ReleaseGoplsTasks) mailAnnouncement(ctx *wf.TaskContext, semv semversion, prerelease, commit string, issue int64) error {
	announce := goplsPrereleaseAnnouncement{
		Version: prerelease,
		Branch:  goplsReleaseBranchName(semv),
		Commit:  commit,
		Issue:   issue,
	}
	content, err := announcementMail(announce)
	if err != nil {
		return err
	}
	ctx.Printf("pre-announcement subject: %s\n\n", content.Subject)
	ctx.Printf("pre-announcement body HTML:\n%s\n", content.BodyHTML)
	ctx.Printf("pre-announcement body text:\n%s", content.BodyText)
	return r.SendMail(r.AnnounceMailHeader, content)
}

func (r *ReleaseGoplsTasks) isValidReleaseVersion(ctx *wf.TaskContext, ver string) (semversion, error) {
	if !semver.IsValid(ver) {
		return semversion{}, fmt.Errorf("the input %q version does not follow semantic version schema", ver)
	}

	versions, err := r.possibleGoplsVersions(ctx)
	if err != nil {
		return semversion{}, fmt.Errorf("failed to get latest Gopls version tags from x/tool: %w", err)
	}

	if !slices.Contains(versions, ver) {
		return semversion{}, fmt.Errorf("the input %q is not next version of any existing versions", ver)
	}

	semver, _ := parseSemver(ver)
	return semver, nil
}

// semversion is a parsed semantic version.
type semversion struct {
	Major, Minor, Patch int
	Pre                 string
}

// parseSemver attempts to parse semver components out of the provided semver
// v. If v is not valid semver in canonical form, parseSemver returns false.
func parseSemver(v string) (_ semversion, ok bool) {
	var parsed semversion
	v, parsed.Pre, _ = strings.Cut(v, "-")
	if _, err := fmt.Sscanf(v, "v%d.%d.%d", &parsed.Major, &parsed.Minor, &parsed.Patch); err == nil {
		ok = true
	}
	return parsed, ok
}

func (s *semversion) prereleaseVersion() (int, error) {
	parts := strings.Split(s.Pre, ".")
	if len(parts) == 1 {
		return 0, fmt.Errorf(`pre-release version does not contain any "."`)
	}

	if len(parts) > 2 {
		return 0, fmt.Errorf(`pre-release version contains %v "."`, len(parts)-1)
	}

	pre, err := strconv.Atoi(parts[1])
	if err != nil {
		return 0, fmt.Errorf("failed to convert pre-release version to int %q: %w", pre, err)
	}

	if pre <= 0 {
		return 0, fmt.Errorf("the pre-release version should be larger than 0: %v", pre)
	}

	return pre, nil
}

// possibleGoplsVersions identifies suitable versions for the upcoming release
// based on the current tags in the repo.
func (r *ReleaseGoplsTasks) possibleGoplsVersions(ctx *wf.TaskContext) ([]string, error) {
	tags, err := r.Gerrit.ListTags(ctx, "tools")
	if err != nil {
		return nil, err
	}

	var semVersions []semversion
	majorMinorPatch := map[int]map[int]map[int]bool{}
	for _, tag := range tags {
		v, ok := strings.CutPrefix(tag, "gopls/")
		if !ok {
			continue
		}

		if !semver.IsValid(v) {
			continue
		}

		// Skip for pre-release versions.
		if semver.Prerelease(v) != "" {
			continue
		}

		semv, ok := parseSemver(v)
		semVersions = append(semVersions, semv)

		if majorMinorPatch[semv.Major] == nil {
			majorMinorPatch[semv.Major] = map[int]map[int]bool{}
		}
		if majorMinorPatch[semv.Major][semv.Minor] == nil {
			majorMinorPatch[semv.Major][semv.Minor] = map[int]bool{}
		}
		majorMinorPatch[semv.Major][semv.Minor][semv.Patch] = true
	}

	var possible []string
	seen := map[string]bool{}
	for _, v := range semVersions {
		nextMajor := fmt.Sprintf("v%d.%d.%d", v.Major+1, 0, 0)
		if _, ok := majorMinorPatch[v.Major+1]; !ok && !seen[nextMajor] {
			seen[nextMajor] = true
			possible = append(possible, nextMajor)
		}

		nextMinor := fmt.Sprintf("v%d.%d.%d", v.Major, v.Minor+1, 0)
		if _, ok := majorMinorPatch[v.Major][v.Minor+1]; !ok && !seen[nextMinor] {
			seen[nextMinor] = true
			possible = append(possible, nextMinor)
		}

		nextPatch := fmt.Sprintf("v%d.%d.%d", v.Major, v.Minor, v.Patch+1)
		if _, ok := majorMinorPatch[v.Major][v.Minor][v.Patch+1]; !ok && !seen[nextPatch] {
			seen[nextPatch] = true
			possible = append(possible, nextPatch)
		}
	}

	semver.Sort(possible)
	return possible, nil
}

// NewReleaseDefinition create a new workflow definition for gopls release.
func (r *ReleaseGoplsTasks) NewReleaseDefinition() *wf.Definition {
	wd := wf.New(wf.ACL{Groups: []string{groups.ToolsTeam}})

	version := wf.Param(wd, wf.ParamDef[string]{Name: "pre-release version"})
	reviewers := wf.Param(wd, reviewersParam)

	semv := wf.Task1(wd, "validating input version", r.isValidPrereleaseVersion, version)
	tagged := wf.Action1(wd, "tag the release", r.tagRelease, semv)

	changeID := wf.Task2(wd, "updating x/tools dependency in master branch in gopls sub dir", r.updateDependencyIfMinor, reviewers, semv, wf.After(tagged))
	_ = wf.Task1(wd, "await x/tools gopls dependency CL submission in gopls sub dir", clAwaiter{r.Gerrit}.awaitSubmission, changeID)

	return wd
}

// isValidPrereleaseVersion verifies the input version satisfies the following
// conditions:
// - It is the latest pre-release version for its Major.Minor.Patch.
// - A corresponding "gopls/vX.Y.Z-pre.V" tag exists in the x/tools repo,
// pointing to the release branch's head commit.
// - The corresponding release tag "gopls/vX.Y.Z" does not exist in the repo.
//
// Returns the parsed semantic pre-release version from the input pre-release
// version string.
func (r *ReleaseGoplsTasks) isValidPrereleaseVersion(ctx *wf.TaskContext, version string) (semversion, error) {
	if !semver.IsValid(version) {
		return semversion{}, fmt.Errorf("the input %q version does not follow semantic version schema", version)
	}

	semv, ok := parseSemver(version)
	if !ok {
		return semversion{}, fmt.Errorf("invalid semantic version %q", version)
	}
	if semv.Pre == "" {
		return semversion{}, fmt.Errorf("the input %q version does not contain any pre-release version.", version)
	}

	pre, err := semv.prereleaseVersion()
	if err != nil {
		return semversion{}, err
	}

	// Verify the release tag is absent.
	tags, err := r.Gerrit.ListTags(ctx, "tools")
	if err != nil {
		return semversion{}, err
	}

	releaseTag := fmt.Sprintf("gopls/v%v.%v.%v", semv.Major, semv.Minor, semv.Patch)
	for _, tag := range tags {
		if tag == releaseTag {
			return semversion{}, fmt.Errorf("this version has been released, the release tag %s already exists.", releaseTag)
		}
	}

	// Check if the provided pre-release is the latest among all existing
	// pre-release versions.
	current, err := currentGoplsPrerelease(ctx, r.Gerrit, semv)
	if err != nil {
		return semversion{}, fmt.Errorf("failed to figure out the current prerelease version for gopls v%v.%v.%v", semv.Major, semv.Minor, semv.Patch)
	}

	if current > pre {
		return semversion{}, fmt.Errorf("there is a newer pre-release version available: %v.%v.%v-pre.%v", semv.Major, semv.Minor, semv.Patch, current)
	}

	if current < pre {
		return semversion{}, fmt.Errorf("input pre-release version %s is not yet available. Latest available is %v.%v.%v-pre.%v", version, semv.Major, semv.Minor, semv.Patch, current)
	}

	// Check if the provided pre-release is the head of the branch.
	branchName := goplsReleaseBranchName(semv)
	head, err := r.Gerrit.ReadBranchHead(ctx, "tools", branchName)
	if err != nil {
		return semversion{}, err
	}

	tagInfo, err := r.Gerrit.GetTag(ctx, "tools", fmt.Sprintf("gopls/%s", version))
	if err != nil {
		return semversion{}, err
	}

	if tagInfo.Revision != head {
		return semversion{}, fmt.Errorf("x/tools branch %s's head commit is %s, but tag %s points to revision %s", branchName, head, fmt.Sprintf("gopls/%s", version), tagInfo.Revision)
	}

	return semv, nil
}

// tagRelease locates the commit associated with the pre-release version and
// applies the official release tag in form of "gopls/vX.Y.Z" to the same commit.
func (r *ReleaseGoplsTasks) tagRelease(ctx *wf.TaskContext, semv semversion) error {
	info, err := r.Gerrit.GetTag(ctx, "tools", fmt.Sprintf("gopls/v%v.%v.%v-%s", semv.Major, semv.Minor, semv.Patch, semv.Pre))
	if err != nil {
		return err
	}

	// Defensively guard against re-creating tags.
	ctx.DisableRetries()

	releaseTag := fmt.Sprintf("gopls/v%v.%v.%v", semv.Major, semv.Minor, semv.Patch)
	if err := r.Gerrit.Tag(ctx, "tools", releaseTag, info.Revision); err != nil {
		return err
	}

	ctx.Printf("tagged commit %s with tag %s", info.Revision, releaseTag)
	return nil
}

// updateDependencyIfMinor update the dependency of x/tools repo in master
// branch.
//
// Returns the change ID.
func (r *ReleaseGoplsTasks) updateDependencyIfMinor(ctx *wf.TaskContext, reviewers []string, semv semversion) (string, error) {
	if semv.Patch != 0 {
		return "", nil
	}

	clTitle := fmt.Sprintf("gopls/go.mod: update dependencies following the v%v.%v.%v release", semv.Major, semv.Minor, semv.Patch)
	openCL, err := openCL(ctx, r.Gerrit, "master", clTitle)
	if err != nil {
		return "", fmt.Errorf("failed to find the open CL of title %q in master branch: %w", clTitle, err)
	}
	if openCL != "" {
		ctx.Printf("not creating CL: found existing CL %s", openCL)
		return openCL, nil
	}

	// TODO(hxjiang): Remove -compat flag once gopls no longer supports building
	// with older Go versions.
	const script = `cd gopls
pwd
go get -u all
go mod tidy -compat=1.19
`
	changed, err := executeAndMonitorChange(ctx, r.CloudBuild, "master", script, []string{"gopls/go.mod", "gopls/go.sum"})
	if err != nil {
		return "", err
	}

	// Skip CL creation as nothing changed.
	if len(changed) == 0 {
		return "", nil
	}

	changeInput := gerrit.ChangeInput{
		Project: "tools",
		Branch:  "master",
		Subject: fmt.Sprintf("%s\n\nThis is an automated CL which updates the go.mod and go.sum.", clTitle),
	}

	ctx.Printf("creating auto-submit change under master branch in x/tools repo.")
	return r.Gerrit.CreateAutoSubmitChange(ctx, changeInput, reviewers, changed)
}

// executeAndMonitorChange runs the specified script on the designated branch,
// tracking changes to the provided files.
//
// Returns a map where keys are the filenames of modified files and values are
// their corresponding content after script execution.
func executeAndMonitorChange(ctx *wf.TaskContext, cloudBuild CloudBuildClient, branch, script string, watchFiles []string) (map[string]string, error) {
	// Checkout to the provided branch.
	fullScript := fmt.Sprintf(`git checkout %s
git rev-parse --abbrev-ref HEAD
git rev-parse --ref HEAD
`, branch)
	// Make a copy of all file that need to watch.
	// If the file does not exist, create a empty file and a empty before file.
	for _, file := range watchFiles {
		if strings.Contains(file, "'") {
			return nil, fmt.Errorf("file name %q contains '", file)
		}
		fullScript += fmt.Sprintf(`if [ -f '%[1]s' ]; then
    cp '%[1]s' '%[1]s.before'
else
    touch '%[1]s' '%[1]s.before'
fi
`, file)
	}
	// Execute the script provided.
	fullScript += script

	// Output files before the script execution and after the script execution.
	outputFiles := []string{}
	for _, file := range watchFiles {
		outputFiles = append(outputFiles, file+".before")
		outputFiles = append(outputFiles, file)
	}
	build, err := cloudBuild.RunScript(ctx, fullScript, "tools", outputFiles)
	if err != nil {
		return nil, err
	}

	outputs, err := buildToOutputs(ctx, cloudBuild, build)
	if err != nil {
		return nil, err
	}

	changed := map[string]string{}
	for i := 0; i < len(outputFiles); i += 2 {
		if before, after := outputs[outputFiles[i]], outputs[outputFiles[i+1]]; before != after {
			changed[outputFiles[i+1]] = after
		}
	}

	return changed, nil
}

// A clAwaiter closes over a GerritClient to provide a reusable workflow task
// for awaiting the submission of a Gerrit change.
type clAwaiter struct {
	GerritClient
}

func (c clAwaiter) awaitSubmission(ctx *wf.TaskContext, changeID string) (string, error) {
	if changeID == "" {
		ctx.Printf("not awaiting: no CL was created")
		return "", nil
	}

	ctx.Printf("awaiting review/submit of %v", ChangeLink(changeID))
	return AwaitCondition(ctx, 10*time.Second, func() (string, bool, error) {
		return c.Submitted(ctx, changeID, "")
	})
}

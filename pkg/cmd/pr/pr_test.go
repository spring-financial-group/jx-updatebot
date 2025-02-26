package pr_test

import (
	"context"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/jenkins-x/jx-helpers/v3/pkg/helmer"
	"github.com/jenkins-x/jx-helpers/v3/pkg/stringhelpers"

	"github.com/jenkins-x-plugins/jx-updatebot/pkg/cmd/pr"
	"github.com/jenkins-x/go-scm/scm"
	"github.com/jenkins-x/go-scm/scm/driver/fake"
	"github.com/jenkins-x/jx-helpers/v3/pkg/cmdrunner"
	"github.com/jenkins-x/jx-helpers/v3/pkg/cmdrunner/fakerunner"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

// FakeGitService implements the scm.GitService interface using fake data
type FakeGitService struct {
	scm.GitService
	Data *fake.Data
}

// ListCommits returns the commits from the fake data's CommitMap for the given repo
func (f *FakeGitService) ListCommits(_ context.Context, repo string, _ scm.CommitListOptions) ([]*scm.Commit, *scm.Response, error) {
	commits, ok := f.Data.CommitMap[repo]
	if !ok || len(commits) == 0 {
		return nil, nil, scm.ErrNotFound
	}
	var result []*scm.Commit
	for i := range commits {
		commit := &commits[i]
		result = append(result, commit)
	}
	return result, nil, nil
}

func TestCreate(t *testing.T) {
	ev := os.Getenv("JX_EXCLUDE_TEST")
	if ev == "" {
		ev = "go"
	}
	excludeTests := strings.Split(ev, ",")
	runner := &fakerunner.FakeRunner{
		CommandRunner: func(c *cmdrunner.Command) (string, error) {
			if c.Name == "git" {
				// intercept push commands as before
				if len(c.Args) > 0 && c.Args[0] == "push" {
					t.Logf("faking command %s in dir %s\n", c.CLI(), c.Dir)
					return "", nil
				}
				// intercept git log commands to simulate a non-merge commit with a parent hash and fake author
				if len(c.Args) > 0 && c.Args[0] == "log" {
					args := strings.Join(c.Args, " ")
					if strings.Contains(args, "--pretty=%P") {
						t.Logf("faking command %s in dir %s\n", c.CLI(), c.Dir)
						// Return the parent commit hash that matches our fake commit data.
						return "parent-sha", nil
					}
					if strings.Contains(args, "--pretty=%an") {
						t.Logf("faking command %s in dir %s\n", c.CLI(), c.Dir)
						// Return the expected commit author.
						return "test-author", nil
					}
				}
			}
			return cmdrunner.DefaultCommandRunner(c)
		},
	}

	fileNames, err := os.ReadDir("test_data")
	assert.NoError(t, err)

	fakeHelmer := helmer.NewFakeHelmer()
	fakeHelmer.ChartsAllVersions["jxgh/jx-build-controller"] = []helmer.ChartSummary{
		{
			ChartVersion: "9.1.2",
		},
	}

	for _, f := range fileNames {
		if !f.IsDir() {
			continue
		}
		name := f.Name()
		if stringhelpers.StringArrayIndex(excludeTests, name) >= 0 {
			t.Logf("excluding test %s\n", name)
			continue
		}
		dir := filepath.Join("test_data", name)
		scmClient, fakeData := fake.NewDefault()

		_, o := pr.NewCmdPullRequest()
		o.Dir = dir
		o.CommandRunner = runner.Run
		o.ScmClient = scmClient
		o.ScmClientFactory.ScmClient = scmClient
		o.ScmClientFactory.NoWriteGitCredentialsFile = true
		o.Helmer = fakeHelmer
		o.Version = "1.2.3"
		o.GitKind = "fake"
		o.EnvironmentPullRequestOptions.ScmClientFactory.GitServerURL = "https://github.com"
		o.EnvironmentPullRequestOptions.ScmClientFactory.GitToken = "dummytoken"
		o.EnvironmentPullRequestOptions.ScmClientFactory.GitUsername = "dummyuser"

		err := o.Run()
		require.NoError(t, err, "failed to run command for test %s", name)

		t.Logf("ran test %s\n", name)

		if name == "versionStream" {
			require.Len(t, fakeData.PullRequests, 1, "should have 1 Pull Request created for %s", name)
		}

		for n, pullRequest := range fakeData.PullRequests {
			t.Logf("test %s created PR #%d with title: %s\n", name, n, pullRequest.Title)
			t.Logf("body: %s\n\n", pullRequest.Body)
		}

	}
}

func TestAssignAuthorToCommit(t *testing.T) {
	fileNames, err := os.ReadDir("test_data")
	assert.NoError(t, err)

	for _, f := range fileNames {
		if !f.IsDir() || f.Name() != "assignauthor" {
			continue
		}

		t.Logf("Running test for %s\n", f.Name())

		dir := filepath.Join("test_data", f.Name())
		fakeScmClient, fakeData := fake.NewDefault()

		// Prepopulate fake data
		fakeData.CommitMap["jx3-gitops-repositories/jx3-kubernetes"] = []scm.Commit{
			{Sha: "dummy-sha", Author: scm.Signature{Login: "irrelevant"}},
			{Sha: "parent-sha", Author: scm.Signature{Login: "test-author"}},
		}

		fakeData.AssigneesAdded = []string{}

		runner := &fakerunner.FakeRunner{
			CommandRunner: func(c *cmdrunner.Command) (string, error) {
				if c.Name == "git" {
					// Intercept push commands
					if len(c.Args) > 0 && c.Args[0] == "push" {
						t.Logf("faking command %s in dir %s\n", c.CLI(), c.Dir)
						return "", nil
					}
					// Intercept log commands
					if len(c.Args) > 0 && c.Args[0] == "log" {
						argsStr := strings.Join(c.Args, " ")
						if strings.Contains(argsStr, "--pretty=%P") {
							t.Logf("faking command %s in dir %s\n", c.CLI(), c.Dir)
							return "parent-sha", nil
						}
						if strings.Contains(argsStr, "--pretty=%an") {
							t.Logf("faking command %s in dir %s\n", c.CLI(), c.Dir)
							return "test-author", nil
						}
					}
				}
				return cmdrunner.DefaultCommandRunner(c)
			},
		}

		// Configure the Options object
		_, o := pr.NewCmdPullRequest()
		o.Dir = dir
		o.CommandRunner = runner.Run
		o.ScmClient = fakeScmClient
		o.ScmClientFactory.ScmClient = fakeScmClient
		o.ScmClientFactory.NoWriteGitCredentialsFile = true
		o.Version = "1.2.3"
		o.EnvironmentPullRequestOptions.ScmClientFactory.GitServerURL = "https://github.com"
		o.EnvironmentPullRequestOptions.ScmClientFactory.GitToken = "dummytoken"
		o.EnvironmentPullRequestOptions.ScmClientFactory.GitUsername = "dummyuser"
		o.GitKind = "fake"

		// Override the Git service to avoid the "implement me" panic
		fakeScmClient.Git = &FakeGitService{Data: fakeData}

		// Run the command
		err = o.Run()
		require.NoError(t, err, "failed to run command for test %s", f.Name())

		// Validate the assignments
		expectedAssignees := []string{"foo", "bar", "test-author"}
		actualAssignees := []string{}
		for _, assignee := range fakeData.AssigneesAdded {
			parts := strings.Split(assignee, ":")
			actualAssignees = append(actualAssignees, parts[1])
		}

		assert.ElementsMatch(t, expectedAssignees, actualAssignees, "PR should include all specified assignees")
		t.Logf("PR created successfully with assignees: %v\n", actualAssignees)
	}
}

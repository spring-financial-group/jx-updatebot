package pr

import (
	"context"
	"fmt"
	"os"
	"path/filepath"
	"strings"

	"github.com/jenkins-x-plugins/jx-gitops/pkg/cmd/git/setup"
	"github.com/jenkins-x/jx-helpers/v3/pkg/gitclient"
	"github.com/jenkins-x/jx-helpers/v3/pkg/helmer"
	"github.com/jenkins-x/jx-helpers/v3/pkg/scmhelpers"
	"github.com/shurcooL/githubv4"

	"github.com/jenkins-x-plugins/jx-promote/pkg/environments"
	"github.com/jenkins-x-plugins/jx-updatebot/pkg/apis/updatebot/v1alpha1"
	"github.com/jenkins-x/go-scm/scm"
	"github.com/jenkins-x/jx-helpers/v3/pkg/cobras/helper"
	"github.com/jenkins-x/jx-helpers/v3/pkg/cobras/templates"
	"github.com/jenkins-x/jx-helpers/v3/pkg/files"
	"github.com/jenkins-x/jx-helpers/v3/pkg/gitclient/gitdiscovery"
	"github.com/jenkins-x/jx-helpers/v3/pkg/options"
	"github.com/jenkins-x/jx-helpers/v3/pkg/stringhelpers"
	"github.com/jenkins-x/jx-helpers/v3/pkg/termcolor"
	"github.com/jenkins-x/jx-helpers/v3/pkg/yamls"
	"github.com/jenkins-x/jx-logging/v3/pkg/log"

	"github.com/spf13/cobra"
)

var (
	info = termcolor.ColorInfo

	cmdLong = templates.LongDesc(`
		Create a Pull Request on each downstream repository
`)
)

// Options the options for the command
type Options struct {
	environments.EnvironmentPullRequestOptions

	Dir                string
	ConfigFile         string
	Version            string
	VersionFile        string
	AddChangelog       string
	GitCommitUsername  string
	GitCommitUserEmail string
	PipelineBaseRef    string
	PipelineCommitSha  string
	PipelineRepoURL    string
	AutoMerge          bool
	NoVersion          bool
	GitCredentials     bool
	PRAssignees        []string
	Labels             []string
	TemplateData       map[string]interface{}
	PullRequestSHAs    map[string]string
	Helmer             helmer.Helmer
	GraphQLClient      *githubv4.Client
	UpdateConfig       v1alpha1.UpdateConfig
}

// NewCmdPullRequest creates a command object for the command
func NewCmdPullRequest() (*cobra.Command, *Options) {
	o := &Options{}

	cmd := &cobra.Command{
		Use:   "pr",
		Short: "Create a Pull Request on each downstream repository",
		Long:  cmdLong,
		Run: func(_ *cobra.Command, _ []string) {
			err := o.Run()
			helper.CheckErr(err)
		},
	}
	cmd.Flags().StringVarP(&o.Dir, "dir", "d", ".", "the directory look for the VERSION file")
	cmd.Flags().StringVarP(&o.ConfigFile, "config-file", "c", "", "the updatebot config file. If none specified defaults to .jx/updatebot.yaml")
	cmd.Flags().StringVarP(&o.Version, "version", "", "", "the version number to promote. If not specified uses $VERSION or the version file")
	cmd.Flags().StringVarP(&o.VersionFile, "version-file", "", "", "the file to load the version from if not specified directly or via a $VERSION environment variable. Defaults to VERSION in the current dir")
	cmd.Flags().StringVarP(&o.Application, "app", "a", "", "the Application to promote. Used for informational purposes")
	cmd.Flags().StringVarP(&o.AddChangelog, "add-changelog", "", "", "a file to take a changelog from to add to the pull request body. Typically a file generated by jx changelog.")
	cmd.Flags().StringVarP(&o.ChangelogSeparator, "changelog-separator", "", os.Getenv("CHANGELOG_SEPARATOR"), "the separator to use between commit message and changelog in the pull request body. Default to ----- or if set the CHANGELOG_SEPARATOR environment variable")
	cmd.Flags().StringVar(&o.CommitTitle, "pull-request-title", "", "the PR title")
	cmd.Flags().StringVar(&o.CommitMessage, "pull-request-body", "", "the PR body")
	cmd.Flags().StringVarP(&o.GitCommitUsername, "git-user-name", "", "", "the user name to git commit")
	cmd.Flags().StringVarP(&o.GitCommitUserEmail, "git-user-email", "", "", "the user email to git commit")
	cmd.Flags().StringVarP(&o.PipelineCommitSha, "pipeline-commit-sha", "", os.Getenv("PULL_BASE_SHA"), "the git SHA of the commit that triggered the pipeline")
	cmd.Flags().StringVarP(&o.PipelineBaseRef, "pipeline-base-ref", "", os.Getenv("PULL_BASE_REF"), "the git base ref of the commit that triggered the pipeline")
	cmd.Flags().StringVarP(&o.PipelineRepoURL, "pipeline-repo-url", "", os.Getenv("REPO_URL"), "the git URL of the repository that triggered the pipeline")
	cmd.Flags().StringSliceVar(&o.Labels, "labels", []string{}, "a list of labels to apply to the PR")
	cmd.Flags().StringSliceVar(&o.PRAssignees, "pull-request-assign", []string{}, "Assignees of created PRs")
	cmd.Flags().BoolVarP(&o.AutoMerge, "auto-merge", "", true, "should we automatically merge if the PR pipeline is green")
	cmd.Flags().BoolVarP(&o.NoVersion, "no-version", "", false, "disables validation on requiring a '--version' option or environment variable to be required")
	cmd.Flags().BoolVarP(&o.GitCredentials, "git-credentials", "", false, "ensures the git credentials are setup so we can push to git")
	o.EnvironmentPullRequestOptions.ScmClientFactory.AddFlags(cmd)

	cmd.Flags().StringVarP(&o.CommitTitle, "commit-title", "", "", "the commit title")
	cmd.Flags().StringVarP(&o.CommitMessage, "commit-message", "", "", "the commit message")
	cmd.Flags().StringVarP(&o.BaseBranchName, "base-branch-name", "b", "", "the base branch name to use for new pull requests")

	return cmd, o
}

// Run implements the command
func (o *Options) Run() error {
	err := o.Validate()
	if err != nil {
		return fmt.Errorf("failed to validate: %w", err)
	}

	// Auto-discover git URL and commit details if not provided
	err = o.SetCommitDetails(o.Dir, o.CommitMessage, o.CommitTitle, o.Application)
	if err != nil {
		return fmt.Errorf("failed to set commit details: %w", err)
	}

	// Handle changelog
	err = o.SetChangeLog(o.AddChangelog)
	if err != nil {
		return fmt.Errorf("failed to set changelog: %w", err)
	}

	BaseBranchName := o.BaseBranchName

	for i, rule := range o.UpdateConfig.Spec.Rules {
		err = o.ProcessRule(&rule, i)
		if err != nil {
			return fmt.Errorf("failed to process rule #%d: %w", i, err)
		}

		err = o.ProcessRuleURLs(&rule, BaseBranchName)
		if err != nil {
			return fmt.Errorf("failed to process URLs: %w", err)
		}
		err = o.CreateOrReusePullRequests(&rule, o.Labels, o.AutoMerge)
		if err != nil {
			return fmt.Errorf("failed to create Pull Requests: %w", err)
		}
	}
	return nil
}

func (o *Options) Validate() error {
	if o.TemplateData == nil {
		o.TemplateData = map[string]interface{}{}
	}
	if o.PullRequestSHAs == nil {
		o.PullRequestSHAs = map[string]string{}
	}
	if o.Version == "" {
		if o.VersionFile == "" {
			o.VersionFile = filepath.Join(o.Dir, "VERSION")
		}
		exists, err := files.FileExists(o.VersionFile)
		if err != nil {
			return fmt.Errorf("failed to check for file %s: %w", o.VersionFile, err)
		}
		if exists {
			data, err := os.ReadFile(o.VersionFile)
			if err != nil {
				return fmt.Errorf("failed to read version file %s: %w", o.VersionFile, err)
			}
			o.Version = strings.TrimSpace(string(data))
		} else {
			log.Logger().Infof("version file %s does not exist", o.VersionFile)
		}
	}
	if o.Version == "" {
		o.Version = os.Getenv("VERSION")
		if o.Version == "" && !o.NoVersion {
			return options.MissingOption("version")
		}
	}

	// lets default the config file
	if o.ConfigFile == "" {
		o.ConfigFile = filepath.Join(o.Dir, ".jx", "updatebot.yaml")
	}
	exists, err := files.FileExists(o.ConfigFile)
	if err != nil {
		return fmt.Errorf("failed to check for file %s: %w", o.ConfigFile, err)
	}
	if exists {
		err = yamls.LoadFile(o.ConfigFile, &o.UpdateConfig)
		if err != nil {
			return fmt.Errorf("failed to load config file %s: %w", o.ConfigFile, err)
		}
	} else {
		log.Logger().Warnf("file %s does not exist so cannot create any updatebot Pull Requests", o.ConfigFile)
	}

	if len(o.Labels) == 0 {
		o.Labels = o.UpdateConfig.Spec.PullRequestLabels
	}

	if o.Helmer == nil {
		o.Helmer = helmer.NewHelmCLIWithRunner(o.CommandRunner, "helm", o.Dir, false)
	}

	// lazy create the git client
	g := o.EnvironmentPullRequestOptions.Git()

	_, _, err = gitclient.EnsureUserAndEmailSetup(g, o.Dir, o.GitCommitUsername, o.GitCommitUserEmail)
	if err != nil {
		return fmt.Errorf("failed to setup git user and email: %w", err)
	}

	// lets try default the git user/token
	if o.ScmClientFactory.GitToken == "" {
		if o.ScmClientFactory.GitServerURL == "" {
			// lets try discover the git URL
			discover := &scmhelpers.Options{
				Dir:             o.Dir,
				GitClient:       o.Git(),
				CommandRunner:   o.CommandRunner,
				DiscoverFromGit: true,
			}
			err := discover.Validate()
			if err != nil {
				return fmt.Errorf("failed to discover repository details: %w", err)
			}
			o.ScmClientFactory.GitServerURL = discover.GitServerURL
			o.ScmClientFactory.GitToken = discover.GitToken
		}
		if o.ScmClientFactory.GitServerURL == "" {
			return fmt.Errorf("no git-server could be found")
		}
		err = o.ScmClientFactory.FindGitToken()
		if err != nil {
			return fmt.Errorf("failed to find git token: %w", err)
		}
	}
	if o.GitCommitUsername == "" {
		o.GitCommitUsername = o.ScmClientFactory.GitUsername
	}
	if o.GitCommitUsername == "" {
		o.GitCommitUsername = os.Getenv("GIT_USERNAME")
	}
	if o.GitCommitUsername == "" {
		o.GitCommitUsername = "jenkins-x-bot"
	}

	if o.GitCredentials {
		if o.ScmClientFactory.GitToken == "" {
			return fmt.Errorf("missing git token environment variable. Try setting GIT_TOKEN or GITHUB_TOKEN")
		}
		_, gc := setup.NewCmdGitSetup()
		gc.Dir = o.Dir
		gc.DisableInClusterTest = true
		gc.UserEmail = o.GitCommitUserEmail
		gc.UserName = o.GitCommitUsername
		gc.Password = o.ScmClientFactory.GitToken
		gc.GitProviderURL = "https://github.com"
		err = gc.Run()
		if err != nil {
			return fmt.Errorf("failed to setup git credentials file: %w", err)
		}
		log.Logger().Infof("setup git credentials file for user %s and email %s", gc.UserName, gc.UserEmail)
	}
	if o.ChangelogSeparator == "" {
		o.ChangelogSeparator = "-----"
	}
	return nil
}

func (o *Options) GetSparseCheckoutPatterns(rule *v1alpha1.Rule) ([]string, error) {
	patterns := make([]string, len(rule.Changes))
	for _, change := range rule.Changes {
		if change.Command != nil {
			return nil, fmt.Errorf("sparse checkout not supported for command change")
		}
		if change.VersionStream != nil {
			return nil, fmt.Errorf("sparse checkout not supported for VersionStream change")
		}
		if change.Go != nil {
			patterns = append(patterns, o.SparseCheckoutPatternsGo()...)
		}
		if change.Regex != nil {
			patterns = append(patterns, o.SparseCheckoutPatternsRegex(change.Regex)...)
		}
	}
	return patterns, nil
}

// ApplyChanges applies the changes to the given dir
func (o *Options) ApplyChanges(dir, gitURL string, change v1alpha1.Change) error {
	if change.Command != nil {
		return o.ApplyCommand(dir, change.Command)
	}
	if change.Go != nil {
		return o.ApplyGo(dir, gitURL, change.Go)
	}
	if change.Regex != nil {
		return o.ApplyRegex(dir, gitURL, change, change.Regex)
	}
	if change.VersionStream != nil {
		return o.ApplyVersionStream(dir, change.VersionStream)
	}
	log.Logger().Infof("ignoring unknown change %#v", change)
	return nil
}

func (o *Options) FindURLs(rule *v1alpha1.Rule) error {
	for _, change := range rule.Changes {
		if change.Go != nil {
			err := o.GoFindURLs(rule, change.Go)
			if err != nil {
				return fmt.Errorf("failed to find go repositories to update: %w", err)
			}

		}
	}
	return nil
}

func (o *Options) SetChangeLog(addChangeLog string) error {
	if addChangeLog != "" {
		changelog, err := os.ReadFile(addChangeLog)
		if err != nil {
			return fmt.Errorf("failed to read changelog file %s: %w", addChangeLog, err)
		}
		o.EnvironmentPullRequestOptions.CommitChangelog = string(changelog)
	}
	return nil
}

// SetCommitDetails discovers the git URL, and sets the application name, commit message and title
func (o *Options) SetCommitDetails(dir, commitMessage, commitTitle, application string) error {
	if commitMessage == "" || commitTitle == "" || application == "" {
		if application == "" || commitMessage == "" {
			{
				gitURL, err := gitdiscovery.FindGitURLFromDir(dir, true)
				if err != nil {
					log.Logger().Warnf("failed to find git URL %s", err.Error())
				} else if gitURL != "" {
					if application == "" {
						gitURLPart := strings.Split(gitURL, "/")
						o.Application = gitURLPart[len(gitURLPart)-2] + "/" +
							strings.TrimSuffix(gitURLPart[len(gitURLPart)-1], ".git")
					}
					if commitMessage == "" {
						o.CommitMessage = fmt.Sprintf("from: %s\n", gitURL)
					}
				}

				if commitTitle == "" {
					if application == "" {
						o.CommitTitle = fmt.Sprintf("chore(deps): upgrade to version %s", o.Version)
					} else {
						o.CommitTitle = fmt.Sprintf("chore(deps): upgrade %s to version %s", o.Application, o.Version)
					}
				}
				return nil
			}
		}
	}
	return nil
}

// ProcessRule sets the Fork and SparseCheckoutPatterns for the given rule
func (o *Options) ProcessRule(rule *v1alpha1.Rule, index int) error {
	err := o.FindURLs(rule)
	if err != nil {
		return fmt.Errorf("failed to find URLs: %w", err)
	}

	o.Fork = rule.Fork
	if len(rule.URLs) == 0 {
		log.Logger().Warnf("no URLs found for rule #%d, skipping...\n", index)
		return nil
	}

	o.EnvironmentPullRequestOptions.SparseCheckoutPatterns = []string{}
	if rule.SparseCheckout {
		o.EnvironmentPullRequestOptions.SparseCheckoutPatterns, err = o.GetSparseCheckoutPatterns(rule)
		if err != nil {
			return fmt.Errorf("error: failed to get sparse checkout patterns for rule #%d, error=%v", index, err)
		}
	}
	return nil
}

// ProcessRuleURLs apply changes to the set of URLs in the given rule
func (o *Options) ProcessRuleURLs(rule *v1alpha1.Rule, baseBranch string) error {
	for _, gitURL := range rule.URLs {
		if gitURL == "" {
			log.Logger().Warnf("skipping empty git URL")
			continue
		}

		o.BranchName = ""
		o.BaseBranchName = baseBranch

		o.Function = func() error {
			dir := o.OutDir
			for _, ch := range rule.Changes {
				err := o.ApplyChanges(dir, gitURL, ch)
				if err != nil {
					return fmt.Errorf("failed to apply change: %w", err)
				}
			}
			return nil
		}
	}
	return nil
}

// CreateOrReusePullRequests creates or reuses a PR on each of the given rule URLs
func (o *Options) CreateOrReusePullRequests(rule *v1alpha1.Rule, labels []string, automerge bool) error {
	for _, ruleURL := range rule.URLs {
		if ruleURL == "" {
			log.Logger().Warnf("skipping empty git URL")
			continue
		}
		if rule.ReusePullRequest {
			if len(o.Labels) == 0 {
				return fmt.Errorf("to be able to reuse pull request you need to supply pullRequestLabels in config file or --labels")
			}
			o.PullRequestFilter = &environments.PullRequestFilter{Labels: []string{}}
			for _, label := range o.Labels {
				o.PullRequestFilter.Labels = stringhelpers.EnsureStringArrayContains(o.PullRequestFilter.Labels, label)
			}
			if o.AutoMerge {
				o.PullRequestFilter.Labels = stringhelpers.EnsureStringArrayContains(o.PullRequestFilter.Labels, environments.LabelUpdatebot)
			}
		}

		pr, err := o.EnvironmentPullRequestOptions.Create(ruleURL, "", labels, automerge)
		if err != nil {
			return fmt.Errorf("failed to create Pull Request on repository %s: %w", ruleURL, err)
		}
		err = o.AssignUsersToPullRequestIssue(rule, pr, ruleURL, o.PipelineRepoURL, o.PipelineCommitSha, o.PipelineBaseRef, o.GitKind)
		if err != nil {
			return fmt.Errorf("failed to assign users to PR: %w", err)
		}
	}
	return nil
}

// AssignUsersToPullRequestIssue assigns user to a downstream PR issue
func (o *Options) AssignUsersToPullRequestIssue(rule *v1alpha1.Rule, pullRequest *scm.PullRequest, ruleURL, pipelineURL, pipelineSHA, pipelineBaseRef, gitKind string) error {
	var assignees []string
	for _, pullRequestAssignee := range rule.PullRequestAssignees {
		assignees = stringhelpers.EnsureStringArrayContains(assignees, pullRequestAssignee)
	}
	if rule.AssignAuthorToPullRequests {
		author, err := o.FindParentCommitAuthor(pipelineURL, pipelineSHA, pipelineBaseRef, gitKind)
		if err != nil {
			return fmt.Errorf("failed to find commit author: %w", err)
		}
		if author != "" {
			assignees = stringhelpers.EnsureStringArrayContains(assignees, author)
		}
	}
	if len(assignees) > 0 {
		err := o.AssignUsersToIssue(pullRequest, assignees, ruleURL, gitKind)
		if err != nil {
			return fmt.Errorf("failed to assign users to PR: %w", err)
		}
	}
	return nil
}

// FindParentCommitAuthor finds the author of the parent commit given current commit SHA
func (o *Options) FindParentCommitAuthor(gitURL, sha, baseRef, gitKind string) (string, error) {
	ctx := context.Background()
	scmClient, repoFullName, err := o.GetScmClient(gitURL, gitKind)
	if err != nil {
		return "", fmt.Errorf("failed to create ScmClient: %w", err)
	}

	// Find the parent commit by listing all commits and choosing commit after the current one
	// Set a reasonable default for returned commit list size
	commitOpts := scm.CommitListOptions{
		Ref:  baseRef,
		Page: 1,
		Size: 50,
		Path: "",
	}
	commits, _, err := scmClient.Git.ListCommits(ctx, repoFullName, commitOpts)
	if err != nil {
		return "", fmt.Errorf("failed to list commits: %w", err)
	}
	if len(commits) < 2 {
		return "", fmt.Errorf("no possible parent commit found for commit %s", sha)
	}
	// Find the current commit Sha from the list of commits
	for i := range len(commits) {
		if commits[i].Sha == sha && i < len(commits)-1 {
			log.Logger().Infof("Found assumed parent commit %s for commit %s", commits[i+1].Sha, sha)
			// Assume the parent commit author is the next in the list
			author := commits[i+1].Author.Login
			if author == "" {
				log.Logger().Warnf("no author found for commit %s", sha)
			}
			return author, nil
		}
	}
	return "", fmt.Errorf("no parent commit found for commit %s", sha)
}

// AssignUsersToIssue adds users as an assignee to the PR Issue
func (o *Options) AssignUsersToIssue(pullRequest *scm.PullRequest, users []string, gitURL, gitKind string) error {
	ctx := context.Background()
	scmClient, repoFullName, err := o.GetScmClient(gitURL, gitKind)
	if err != nil {
		return fmt.Errorf("failed to create ScmClient: %w", err)
	}
	_, err = scmClient.PullRequests.AssignIssue(ctx, repoFullName, pullRequest.Number, users)
	if err != nil {
		return fmt.Errorf("failed to assign user to PR %d: %w", pullRequest.Number, err)
	}
	return nil
}

package main

import (
	"context"
	"crypto/md5"
	"encoding/json"
	"fmt"
	"io/ioutil"
	"net/http"
	"os"
	"os/user"
	"path"
	"regexp"
	"strings"
	"sync"

	"gopkg.in/src-d/go-git.v4/plumbing"

	"golang.org/x/oauth2"
	"gopkg.in/src-d/go-git.v4/plumbing/object"
	"gopkg.in/src-d/go-git.v4/plumbing/transport/ssh"
	"gopkg.in/src-d/go-git.v4/storage/memory"

	"github.com/BurntSushi/toml"
	"github.com/google/go-github/github"
	flags "github.com/jessevdk/go-flags"
	log "github.com/sirupsen/logrus"
	git "gopkg.in/src-d/go-git.v4"
)

// Leak represents a leaked secret or regex match. This will be output to stdout and/or the report
type Leak struct {
	Line     string `json:"line"`
	Commit   string `json:"commit"`
	Offender string `json:"string"`
	Type     string `json:"reason"`
	Message  string `json:"commitMsg"`
	Author   string `json:"author"`
	File     string `json:"file"`
	Branch   string `json:"branch"`
}

// Repo contains the actual git repository and meta data about the repo
type Repo struct {
	path       string
	url        string
	name       string
	leaks      []Leak
	repository *git.Repository
}

// Owner contains a collection of repos. This could represent an org or user.
type Owner struct {
	path  string
	url   string
	repos []Repo
}

// Options for gitleaks
type Options struct {
	// remote target options
	Repo           string `short:"r" long:"repo" description:"Repo url to audit"`
	GithubUser     string `long:"github-user" description:"User url to audit"`
	GithubOrg      string `long:"github-org" description:"Organization url to audit"`
	IncludePrivate bool   `long:"private" description:"Include private repos in audit"`

	/*
		GitLabUser string `long:"gitlab-user" description:"User url to audit"`
		GitLabOrg  string `long:"gitlab-org" description:"Organization url to audit"`
	*/

	Branch string `short:"b" long:"branch" description:"branch name to audit (defaults to HEAD)"`
	Commit string `long:"commit" description:"sha of commit to stop at"`

	// local target option
	RepoPath  string `long:"repo-path" description:"Path to repo"`
	OwnerPath string `long:"owner-path" description:"Path to owner directory (repos discovered)"`

	// Process options
	MaxGoRoutines    int    `long:"max-go" description:"Maximum number of concurrent go-routines gitleaks spawns"`
	InMem            bool   `long:"in-memory" description:"Run gitleaks in memory"`
	AuditAllBranches bool   `long:"branches-all" description:"run audit on all branches"`
	SingleSearch     string `long:"single-search" description:"single regular expression to search for"`
	ConfigPath       string `long:"config" description:"path to gitleaks config"`
	SSHKey           string `long:"ssh-key" description:"path to ssh key"`

	// Output options
	LogLevel string `long:"log-level" description:"log level"`
	Verbose  bool   `short:"v" long:"verbose" description:"Show verbose output from gitleaks audit"`
	Report   string `long:"report" description:"path to report"`
	Redact   string `long:"redact" description:"redact secrets from log messages and report"`
}

// Config struct for regexes matching and whitelisting
type Config struct {
	Regexes []struct {
		Description string
		Regex       string
	}
	Whitelist struct {
		Files    []string
		Regexes  []string
		Commits  []string
		Branches []string
		Messages []string
	}
}

const defaultConfig = `
title = "gitleaks config"
# add regexes to the regex table
[[regexes]]
description = "AWS"
regex = '''AKIA[0-9A-Z]{16}'''
[[regexes]]
description = "RKCS8"
regex = '''-----BEGIN PRIVATE KEY-----'''
[[regexes]]
description = "RSA"
regex = '''-----BEGIN RSA PRIVATE KEY-----'''
[[regexes]]
description = "Github"
regex = '''(?i)github.*['\"][0-9a-zA-Z]{35,40}['\"]'''
[[regexes]]
description = "SSH"
regex = '''-----BEGIN OPENSSH PRIVATE KEY-----'''
[[regexes]]
description = "Facebook"
regex = '''(?i)facebook.*['\"][0-9a-f]{32}['\"]'''
[[regexes]]
description = "Facebook"
regex = '''(?i)twitter.*['\"][0-9a-zA-Z]{35,44}['\"]'''

[whitelist]

#regexes = [
#  "AKAIMYFAKEAWKKEY",
#]

#files = [
#  "(.*?)(jpg|gif|doc|pdf|bin)$"
#]

#commits = [
#  "BADHA5H1",
#  "BADHA5H2",
#]

#messages = [
#	"eat more veggies"
#	"call your mom"
#	"stretch"
#	"no soda!"
#	"adding tests"
#]

#branches = [
#	"dev/STUPDIFKNFEATURE"
#]
`

var (
	opts              Options
	regexes           map[string]*regexp.Regexp
	singleSearchRegex *regexp.Regexp
	whiteListRegexes  []*regexp.Regexp
	whiteListFiles    []*regexp.Regexp
	whiteListCommits  map[string]bool
	whiteListMessages map[string]bool
	whiteListBranches []string
	fileDiffRegex     *regexp.Regexp
	sshAuth           *ssh.PublicKeys
	dir               string
)

func init() {
	log.SetOutput(os.Stdout)
	regexes = make(map[string]*regexp.Regexp)
}

func main() {
	var (
		leaks []Leak
		repos []Repo
	)
	_, err := flags.Parse(&opts)
	if err != nil {
		os.Exit(1)
	}
	setLogLevel()

	err = optsGuard()
	if err != nil {
		log.Fatal(err)
	}

	err = loadToml()
	if err != nil {
		log.Fatal(err)
	}

	if opts.IncludePrivate {
		// if including private repos use ssh as authentication
		sshAuth, err = getSSHAuth()
		if err != nil {
			log.Fatal(err)
		}
	}

	if !opts.InMem {
		// temporary directory where all the gitleaks plain clones will reside
		dir, err = ioutil.TempDir("", "gitleaks")
		defer os.RemoveAll(dir)
		if err != nil {
			panic(err)
		}
	}

	// start audits
	if opts.Repo != "" || opts.RepoPath != "" {
		r, err := getRepo()
		if err != nil {
			log.Fatal(err)
		}
		repos = append(repos, r)
	} else if ownerTarget() {
		repos, err = getOwnerRepos()
	}
	for _, r := range repos {
		l, err := auditRepo(r.repository)
		if err != nil {
			log.Fatal(err)
		}
		leaks = append(leaks, l...)
	}

	if opts.Report != "" {
		writeReport(leaks)
	}

	if err != nil {
		log.Fatal(err)
	}
}

func writeReport(leaks []Leak) error {
	reportJSON, _ := json.MarshalIndent(leaks, "", "\t")
	err := ioutil.WriteFile(opts.Report, reportJSON, 0644)
	return err
}

// getRepo is responsible for cloning a repository specified in opts.
func getRepo() (Repo, error) {
	var (
		err error
		r   *git.Repository
	)

	if opts.InMem {
		if opts.IncludePrivate {
			r, err = git.Clone(memory.NewStorage(), nil, &git.CloneOptions{
				URL:      opts.Repo,
				Progress: os.Stdout,
				Auth:     sshAuth,
			})
		} else {
			r, err = git.Clone(memory.NewStorage(), nil, &git.CloneOptions{
				URL:      opts.Repo,
				Progress: os.Stdout,
			})
		}
	} else if opts.RepoPath != "" {
		// use existing repo
		r, err = git.PlainOpen(opts.RepoPath)
	} else {
		cloneTarget := fmt.Sprintf("%s/%x", dir, md5.Sum([]byte(fmt.Sprintf("%s%s", opts.GithubUser, opts.Repo))))
		if opts.IncludePrivate {
			r, err = git.PlainClone(cloneTarget, false, &git.CloneOptions{
				URL:      opts.Repo,
				Progress: os.Stdout,
				Auth:     sshAuth,
			})
		} else {
			r, err = git.PlainClone(cloneTarget, false, &git.CloneOptions{
				URL:      opts.Repo,
				Progress: os.Stdout,
			})
		}
	}
	if err != nil {
		return Repo{}, err
	}
	return Repo{
		repository: r,
		path:       opts.RepoPath,
		url:        opts.Repo,
	}, nil
}

func auditBranch(r *git.Repository, ref *plumbing.Reference, leaks []Leak, commitWg *sync.WaitGroup, commitChan chan []Leak) error {
	var (
		err             error
		prevTree        *object.Tree
		limitGoRoutines bool
		semaphore       chan bool
	)

	// goroutine limiting
	if opts.MaxGoRoutines != 0 {
		semaphore = make(chan bool, opts.MaxGoRoutines)
		limitGoRoutines = true
	}
	cIter, err := r.Log(&git.LogOptions{From: ref.Hash()})
	if err != nil {
		return err
	}
	err = cIter.ForEach(func(c *object.Commit) error {
		if limitGoRoutines {
			semaphore <- true
		}
		commitWg.Add(1)
		go func(c *object.Commit, prevTree *object.Tree) {

			var leaksL []Leak
			tree, err := c.Tree()
			if err != nil {
				if limitGoRoutines {
					<-semaphore
				}
				commitChan <- nil
				log.Error("unable to get tree for commit %s, err: %v", c.Hash, err)
				return
			}
			treeChanges, err := tree.Diff(prevTree)
			if err != nil {
				if limitGoRoutines {
					<-semaphore
				}
				commitChan <- nil
				log.Error("unable to get tree for commit %s, err: %v", c.Hash, err)
				return
			}

			patch, _ := treeChanges.Patch()

			var filePath string
			filePatches := patch.FilePatches()
			for _, f := range filePatches {
				skipFile := false
				from, to := f.Files()
				if from != nil {
					filePath = from.Path()
				} else if to != nil {
					filePath = to.Path()
				} else {
					log.Debug("unable to determine file for commit %s", c.Hash)
					filePath = ""
				}
				for _, re := range whiteListFiles {
					if re.FindString(filePath) != "" {
						skipFile = true
					}
				}
				if skipFile {
					continue
				}

				chunks := f.Chunks()
				for _, chunk := range chunks {
					leaksL = append(leaksL, checkDiff(chunk.Content(), c, filePath, string(ref.Name()))...)
				}
			}
			if limitGoRoutines {
				<-semaphore
			}
			commitChan <- leaksL
		}(c, prevTree)

		prevTree, _ = c.Tree()
		return nil
	})
	return nil
}

// auditRepo performs an audit on a repository checking for regex matching and ignoring
// files and regexes that are whitelisted
func auditRepo(r *git.Repository) ([]Leak, error) {
	var (
		err      error
		leaks    []Leak
		commitWg sync.WaitGroup
	)

	ref, err := r.Head()
	if err != nil {
		return leaks, err
	}

	// leak messaging
	commitChan := make(chan []Leak, 1)

	if opts.AuditAllBranches || opts.Branch != "" {
		skipBranch := false
		refs, err := r.Storer.IterReferences()
		if err != nil {
			return leaks, err
		}
		err = refs.ForEach(func(ref *plumbing.Reference) error {
			for _, b := range whiteListBranches {
				if strings.HasSuffix(string(ref.Name()), b) {
					skipBranch = true
				}
			}
			if skipBranch {
				skipBranch = false
				return nil
			}
			auditBranch(r, ref, leaks, &commitWg, commitChan)
			return nil
		})
	} else {
		auditBranch(r, ref, leaks, &commitWg, commitChan)
	}

	go func() {
		for commitLeaks := range commitChan {
			if commitLeaks != nil {
				for _, leak := range commitLeaks {
					leaks = append(leaks, leak)
				}

			}
			commitWg.Done()
		}
	}()

	commitWg.Wait()

	return leaks, err
}

// checkDiff accepts a string diff and commit object then performs a
// regex check
func checkDiff(diff string, commit *object.Commit, filePath string, branch string) []Leak {
	lines := strings.Split(diff, "\n")
	var (
		leaks       []Leak
		ignoreMatch bool
	)

	for _, line := range lines {
		for leakType, re := range regexes {
			ignoreMatch = false
			match := re.FindString(line)
			if match == "" {
				continue
			}

			// whitelists
			for _, wRe := range whiteListRegexes {
				whitelistMatch := wRe.FindString(line)
				if whitelistMatch != "" {
					ignoreMatch = true
				}
			}
			if ignoreMatch {
				continue
			}

			leak := Leak{
				Line:     line,
				Commit:   commit.Hash.String(),
				Offender: match,
				Type:     leakType,
				Message:  commit.Message,
				Author:   commit.Author.String(),
				File:     filePath,
				Branch:   branch,
			}
			leak.log()
			leaks = append(leaks, leak)
		}
	}
	return leaks
}

// auditOwner audits all of the owner's(user or org) repos
func getOwnerRepos() ([]Repo, error) {
	var (
		err   error
		repos []Repo
	)
	ctx := context.Background()

	if opts.OwnerPath != "" {
		repos, err = discoverRepos(opts.OwnerPath)
	} else if opts.GithubOrg != "" {
		githubClient := github.NewClient(githubToken())
		githubOptions := github.RepositoryListByOrgOptions{
			ListOptions: github.ListOptions{PerPage: 10},
		}
		repos, err = getOrgGithubRepos(ctx, &githubOptions, githubClient)
	} else if opts.GithubUser != "" {
		githubClient := github.NewClient(githubToken())
		githubOptions := github.RepositoryListOptions{
			Affiliation: "owner",
			ListOptions: github.ListOptions{
				PerPage: 10,
			},
		}
		repos, err = getUserGithubRepos(ctx, &githubOptions, githubClient)
	}

	return repos, err
}

// getUserGithubRepos
func getUserGithubRepos(ctx context.Context, listOpts *github.RepositoryListOptions, client *github.Client) ([]Repo, error) {
	var (
		err   error
		repos []Repo
		r     *git.Repository
		rs    []*github.Repository
		resp  *github.Response
	)

	for {
		if opts.IncludePrivate {
			rs, resp, err = client.Repositories.List(ctx, "", listOpts)
		} else {
			rs, resp, err = client.Repositories.List(ctx, opts.GithubUser, listOpts)
		}

		for _, rDesc := range rs {
			log.Debugf("Cloning: %s from %s", *rDesc.Name, *rDesc.SSHURL)
			if opts.InMem {
				if opts.IncludePrivate {
					r, err = git.Clone(memory.NewStorage(), nil, &git.CloneOptions{
						URL:  *rDesc.SSHURL,
						Auth: sshAuth,
					})
				} else {
					r, err = git.Clone(memory.NewStorage(), nil, &git.CloneOptions{
						URL: *rDesc.CloneURL,
					})
				}
			} else {
				ownerDir, err := ioutil.TempDir(dir, opts.GithubUser)
				if err != nil {
					return repos, fmt.Errorf("unable to generater owner temp dir: %v", err)
				}
				if opts.IncludePrivate {
					r, err = git.PlainClone(fmt.Sprintf("%s/%s", ownerDir, *rDesc.Name), false, &git.CloneOptions{
						URL:  *rDesc.SSHURL,
						Auth: sshAuth,
					})
				} else {
					r, err = git.PlainClone(fmt.Sprintf("%s/%s", ownerDir, *rDesc.Name), false, &git.CloneOptions{
						URL: *rDesc.CloneURL,
					})

				}
			}
			if err != nil {
				return repos, fmt.Errorf("problem cloning %s -- %v", *rDesc.Name, err)
			}
			repos = append(repos, Repo{
				name:       *rDesc.Name,
				url:        *rDesc.SSHURL,
				repository: r,
			})
		}
		if resp.NextPage == 0 {
			break
		}
		listOpts.Page = resp.NextPage
	}
	return repos, err
}

// getOrgGithubRepos
func getOrgGithubRepos(ctx context.Context, listOpts *github.RepositoryListByOrgOptions, client *github.Client) ([]Repo, error) {
	var (
		err      error
		repos    []Repo
		r        *git.Repository
		ownerDir string
	)

	for {
		// iterate through organization's repo descriptors, open git repos on disk or in mem
		// depending on what options have been set
		rs, resp, err := client.Repositories.ListByOrg(ctx, opts.GithubOrg, listOpts)
		for _, rDesc := range rs {
			log.Debugf("Cloning: %s from %s", *rDesc.Name, *rDesc.SSHURL)
			if opts.InMem {
				if opts.IncludePrivate {
					if sshAuth == nil {
						return nil, fmt.Errorf("no ssh auth available")
					}
					r, err = git.Clone(memory.NewStorage(), nil, &git.CloneOptions{
						URL:  *rDesc.SSHURL,
						Auth: sshAuth,
					})
				} else {
					r, err = git.Clone(memory.NewStorage(), nil, &git.CloneOptions{
						URL: *rDesc.CloneURL,
					})
				}
			} else {
				ownerDir, err = ioutil.TempDir(dir, opts.GithubUser)
				if err != nil {
					return repos, fmt.Errorf("unable to generater owner temp dir: %v", err)
				}
				if opts.IncludePrivate {
					if sshAuth == nil {
						return nil, fmt.Errorf("no ssh auth available")
					}
					r, err = git.PlainClone(fmt.Sprintf("%s/%s", ownerDir, *rDesc.Name), false, &git.CloneOptions{
						URL:  *rDesc.SSHURL,
						Auth: sshAuth,
					})
				} else {
					r, err = git.PlainClone(fmt.Sprintf("%s/%s", ownerDir, *rDesc.Name), false, &git.CloneOptions{
						URL: *rDesc.CloneURL,
					})

				}
			}
			if err != nil {
				return nil, err
			}
			repos = append(repos, Repo{
				url:        *rDesc.SSHURL,
				name:       *rDesc.Name,
				repository: r,
			})
		}
		if err != nil {
			return nil, err
		} else if resp.NextPage == 0 {
			break
		}
		listOpts.Page = resp.NextPage
	}

	return repos, err
}

// gets github client
func githubToken() *http.Client {
	githubToken := os.Getenv("GITHUB_TOKEN")
	if githubToken == "" {
		return nil
	}
	ts := oauth2.StaticTokenSource(
		&oauth2.Token{AccessToken: githubToken},
	)
	return oauth2.NewClient(context.Background(), ts)
}

// discoverRepos looks navigates all the directories of `path`. If a child directory
// contain a .git file then that repo will be added
func discoverRepos(ownerPath string) ([]Repo, error) {
	var (
		err   error
		repos []Repo
	)
	files, err := ioutil.ReadDir(ownerPath)
	if err != nil {
		return repos, err
	}
	for _, f := range files {
		if f.IsDir() {
			repoPath := path.Join(ownerPath, f.Name())
			r, err := git.PlainOpen(repoPath)
			if err != nil {
				continue
			}
			repos = append(repos, Repo{
				repository: r,
				name:       f.Name(),
				path:       repoPath,
			})
		}
	}
	return repos, err
}

// setLogLevel sets log level for gitleaks. Default is Warning
func setLogLevel() {
	switch opts.LogLevel {
	case "info":
		log.SetLevel(log.InfoLevel)
	case "debug":
		log.SetLevel(log.DebugLevel)
	case "warn":
		log.SetLevel(log.WarnLevel)
	default:
		log.SetLevel(log.WarnLevel)
	}
}

// optsGuard prevents invalid options
func optsGuard() error {
	var err error
	if opts.GithubOrg != "" && opts.GithubUser != "" {
		return fmt.Errorf("github user and organization set")
	} else if opts.GithubOrg != "" && opts.OwnerPath != "" {
		return fmt.Errorf("github organization set and local owner path")
	} else if opts.GithubUser != "" && opts.OwnerPath != "" {
		return fmt.Errorf("github user set and local owner path")
	} else if opts.IncludePrivate && os.Getenv("GITHUB_TOKEN") == "" && (opts.GithubOrg != "" || opts.GithubUser != "") {
		return fmt.Errorf("user/organization private repos require env var GITHUB_TOKEN to be set")
	}

	if opts.SingleSearch != "" {
		singleSearchRegex, err = regexp.Compile(opts.SingleSearch)
		if err != nil {
			return fmt.Errorf("unable to compile regex: %s, %v", opts.SingleSearch, err)
		}
	}

	return nil
}

// loadToml loads of the toml config containing regexes.
// 1. look for config path
// 2. two, look for gitleaks config env var
func loadToml() error {
	var (
		config     Config
		configPath string
	)
	if opts.ConfigPath != "" {
		configPath = opts.ConfigPath
		_, err := os.Stat(configPath)
		if err != nil {
			return fmt.Errorf("no gitleaks config at %s", configPath)
		}
	} else {
		configPath = os.Getenv("GITLEAKS_CONFIG")
	}

	if configPath != "" {
		if _, err := toml.DecodeFile(configPath, &config); err != nil {
			return fmt.Errorf("problem loading config: %v", err)
		}
	} else {
		_, err := toml.Decode(defaultConfig, &config)
		if err != nil {
			return fmt.Errorf("problem loading default config: %v", err)
		}

	}

	// load up regexes
	if singleSearchRegex != nil {
		// single search takes precedence over default regex
		regexes["singleSearch"] = singleSearchRegex
	} else {
		for _, regex := range config.Regexes {
			regexes[regex.Description] = regexp.MustCompile(regex.Regex)
		}
	}
	whiteListBranches = config.Whitelist.Branches
	for _, commit := range config.Whitelist.Commits {
		whiteListCommits[commit] = true
	}
	for _, message := range config.Whitelist.Messages {
		whiteListCommits[message] = true
	}
	for _, regex := range config.Whitelist.Files {
		whiteListFiles = append(whiteListFiles, regexp.MustCompile(regex))
	}
	for _, regex := range config.Whitelist.Regexes {
		whiteListRegexes = append(whiteListRegexes, regexp.MustCompile(regex))
	}

	return nil
}

// ownerTarget checks if we are dealing with a remote owner
func ownerTarget() bool {
	if opts.GithubOrg != "" ||
		opts.GithubUser != "" ||
		opts.OwnerPath != "" {
		return true
	}
	return false
}

// getSSHAuth generates ssh auth
func getSSHAuth() (*ssh.PublicKeys, error) {
	var (
		sshKeyPath string
	)
	if opts.SSHKey != "" {
		sshKeyPath = opts.SSHKey
	} else {
		c, _ := user.Current()
		sshKeyPath = fmt.Sprintf("%s/.ssh/id_rsa", c.HomeDir)
	}
	sshAuth, err := ssh.NewPublicKeysFromFile("git", sshKeyPath, "")
	if err != nil {
		return nil, fmt.Errorf("unable to generate ssh key: %v", err)
	}
	return sshAuth, err
}

func (leak *Leak) log() {
	b, _ := json.MarshalIndent(leak, "", "   ")
	fmt.Println(string(b))
}

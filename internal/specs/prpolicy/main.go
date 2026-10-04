// Command prpolicy checks one pull request against the rules of
// docs/SPEC-TEMPLATE.md. The workflow .github/workflows/pr-policy.yml runs it
// from the base branch. It reads the pull request as data, through the GitHub
// API. It never checks out or runs the code of the pull request.
//
// The environment gives the input, as GitHub Actions sets it:
//
//	GITHUB_EVENT_PATH  the file with the pull request event
//	GITHUB_TOKEN       a token that can read the repository and its pull requests
//	GITHUB_API_URL     the root of the API (default https://api.github.com)
package main

import (
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"net/url"
	"os"
	"strings"
	"time"

	"github.com/gezibash/arc/internal/specs"
)

type event struct {
	PullRequest struct {
		Number int    `json:"number"`
		Body   string `json:"body"`
		Base   side   `json:"base"`
		Head   side   `json:"head"`
	} `json:"pull_request"`
}

type side struct {
	SHA  string `json:"sha"`
	Repo *struct {
		FullName string `json:"full_name"`
	} `json:"repo"`
}

type api struct {
	root, token string
	client      *http.Client
	// held keeps each file that was read, so one file costs one request.
	held map[string]*string
}

// get reads one API resource. It returns nil for a resource that does not
// exist.
func (a *api) get(path, accept string) ([]byte, error) {
	request, err := http.NewRequest(http.MethodGet, a.root+path, nil)
	if err != nil {
		return nil, err
	}
	request.Header.Set("Accept", accept)
	request.Header.Set("X-GitHub-Api-Version", "2022-11-28")
	if a.token != "" {
		request.Header.Set("Authorization", "Bearer "+a.token)
	}
	response, err := a.client.Do(request)
	if err != nil {
		return nil, err
	}
	defer func() { _ = response.Body.Close() }()
	body, err := io.ReadAll(response.Body)
	if err != nil {
		return nil, err
	}
	switch response.StatusCode {
	case http.StatusOK:
		return body, nil
	case http.StatusNotFound:
		return nil, nil
	}
	return nil, fmt.Errorf("GET %s: %s: %s", path, response.Status, strings.TrimSpace(string(body)))
}

// file reads one file of a repository at a commit.
func (a *api) file(repo, sha, path string) (string, bool, error) {
	key := repo + "@" + sha + ":" + path
	if text, ok := a.held[key]; ok {
		if text == nil {
			return "", false, nil
		}
		return *text, true, nil
	}
	var escaped []string
	for part := range strings.SplitSeq(path, "/") {
		escaped = append(escaped, url.PathEscape(part))
	}
	body, err := a.get("/repos/"+repo+"/contents/"+strings.Join(escaped, "/")+"?ref="+url.QueryEscape(sha), "application/vnd.github.raw+json")
	if err != nil {
		return "", false, err
	}
	if body == nil {
		a.held[key] = nil
		return "", false, nil
	}
	text := string(body)
	a.held[key] = &text
	return text, true, nil
}

// changes lists the changed files of a pull request.
func (a *api) changes(repo string, number int) ([]specs.Change, error) {
	var all []specs.Change
	for page := 1; ; page++ {
		body, err := a.get(fmt.Sprintf("/repos/%s/pulls/%d/files?per_page=100&page=%d", repo, number, page), "application/vnd.github+json")
		if err != nil {
			return nil, err
		}
		if body == nil {
			return nil, fmt.Errorf("pull request %d of %s does not exist", number, repo)
		}
		var files []struct {
			Filename string `json:"filename"`
			Previous string `json:"previous_filename"`
			Status   string `json:"status"`
		}
		if err := json.Unmarshal(body, &files); err != nil {
			return nil, err
		}
		for _, file := range files {
			all = append(all, specs.Change{Path: file.Filename, Previous: file.Previous, Removed: file.Status == "removed"})
		}
		if len(files) < 100 {
			return all, nil
		}
	}
}

func run() ([]string, error) {
	data, err := os.ReadFile(os.Getenv("GITHUB_EVENT_PATH"))
	if err != nil {
		return nil, fmt.Errorf("GITHUB_EVENT_PATH: %w", err)
	}
	var held event
	if err := json.Unmarshal(data, &held); err != nil {
		return nil, fmt.Errorf("the event is not JSON: %w", err)
	}
	pr := held.PullRequest
	if pr.Number == 0 || pr.Base.Repo == nil || pr.Base.SHA == "" || pr.Head.SHA == "" {
		return nil, fmt.Errorf("the event holds no pull request")
	}
	// The repository of a pull request from a deleted fork is gone. Its
	// commits stay readable through the base repository.
	headRepo := pr.Base.Repo.FullName
	if pr.Head.Repo != nil {
		headRepo = pr.Head.Repo.FullName
	}

	root := os.Getenv("GITHUB_API_URL")
	if root == "" {
		root = "https://api.github.com"
	}
	github := &api{root: root, token: os.Getenv("GITHUB_TOKEN"), client: &http.Client{Timeout: 30 * time.Second}, held: map[string]*string{}}
	changes, err := github.changes(pr.Base.Repo.FullName, pr.Number)
	if err != nil {
		return nil, err
	}

	// A read that fails must fail the check. It must not read as "no file".
	var failed error
	read := func(repo, sha string) func(string) (string, bool) {
		return func(path string) (string, bool) {
			text, ok, err := github.file(repo, sha, path)
			if err != nil && failed == nil {
				failed = err
			}
			return text, ok
		}
	}
	problems := specs.Check(specs.PullRequest{
		Body:    pr.Body,
		Changes: changes,
		Base:    read(pr.Base.Repo.FullName, pr.Base.SHA),
		Head:    read(headRepo, pr.Head.SHA),
	})
	return problems, failed
}

func main() {
	problems, err := run()
	if err != nil {
		fmt.Fprintln(os.Stderr, "prpolicy:", err)
		os.Exit(2)
	}
	for _, problem := range problems {
		// GitHub Actions shows a line that starts with ::error:: on the pull request.
		fmt.Printf("::error::%s\n", problem)
	}
	if len(problems) != 0 {
		fmt.Printf("Spec policy: %d problems. See docs/SPEC-TEMPLATE.md.\n", len(problems))
		os.Exit(1)
	}
	fmt.Println("Spec policy: passed")
}

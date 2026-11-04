package github

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"net/http"
	"net/url"
	"strings"

	"github.com/BingyanStudio/CloudPilot/internal/model"
)

// Issue contains the GitHub fields used to decide whether work is still eligible.
type Issue struct {
	ID          int64           `json:"id"`
	Number      int             `json:"number"`
	State       string          `json:"state"`
	PullRequest json.RawMessage `json:"pull_request"`
	Assignees   []struct {
		Login string `json:"login"`
	} `json:"assignees"`
	Labels []struct {
		Name string `json:"name"`
	} `json:"labels"`
}

// Matches accepts an open issue with any configured assignee or label.
func (issue Issue) Matches(spec model.GitHubInputSpec) bool {
	if issue.State != "open" || len(issue.PullRequest) != 0 {
		return false
	}
	for _, expected := range spec.Assignees {
		for _, assignee := range issue.Assignees {
			if expected == "*" || strings.EqualFold(expected, assignee.Login) {
				return true
			}
		}
	}
	for _, expected := range spec.Labels {
		for _, label := range issue.Labels {
			if strings.EqualFold(expected, label.Name) {
				return true
			}
		}
	}
	return false
}

// FetchIssue reads a single issue without treating inaccessible resources as closed.
func FetchIssue(ctx context.Context, client *http.Client, repository string, number int, token string) (Issue, error) {
	var issue Issue
	parts := strings.Split(repository, "/")
	if len(parts) != 2 || parts[0] == "" || parts[1] == "" || number < 1 {
		return issue, errors.New("invalid GitHub issue target")
	}
	endpoint := fmt.Sprintf("https://api.github.com/repos/%s/%s/issues/%d", url.PathEscape(parts[0]), url.PathEscape(parts[1]), number)
	data, err := request(ctx, client, http.MethodGet, endpoint, token, nil)
	if err != nil {
		return issue, err
	}
	if json.Unmarshal(data, &issue) != nil || issue.ID < 1 || issue.Number != number || (issue.State != "open" && issue.State != "closed") {
		return issue, errors.New("GitHub returned an invalid issue")
	}
	return issue, nil
}

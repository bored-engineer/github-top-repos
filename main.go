package main

import (
	"context"
	"encoding/csv"
	"fmt"
	"iter"
	"log"
	"net/http"
	"os"
	"os/signal"
	"path/filepath"
	"strconv"
	"strings"
	"time"

	ghauth "github.com/bored-engineer/github-auth-http-transport"
	"github.com/shurcooL/githubv4"
)

// Repository is a struct that represents a GitHub repository.
type Repository struct {
	NameWithOwner  string
	CreatedAt      githubv4.DateTime
	PushedAt       githubv4.DateTime
	StargazerCount int
}

// Search runs a GitHub search query using GraphQL for a single page of 100 repository results
// It sorts by the 'updated' date (oldest results first) which is actually the 'pushedAt' timestamp
// It filters results to only include repositories pushed after the provided 'since' time (inclusive)
func Search(
	ctx context.Context,
	client *githubv4.Client,
	query string,
	since *time.Time,
) ([]Repository, bool, error) {
	var results struct {
		Search struct {
			Nodes []struct {
				Repository Repository `graphql:"... on Repository"`
			}
			PageInfo struct {
				HasNextPage bool
			}
		} `graphql:"search(query: $query, type: REPOSITORY, first: 100)"`
	}
	query += " sort:updated-asc"
	if since != nil {
		query += " pushed:>=" + since.Format("2006-01-02T15:04:05Z")
	}
	log.Printf("searching: %s", query)
	if err := client.Query(ctx, &results, map[string]any{
		"query": githubv4.String(query),
	}); err != nil {
		return nil, false, err
	}
	repos := make([]Repository, 0, len(results.Search.Nodes))
	for _, node := range results.Search.Nodes {
		repos = append(repos, node.Repository)
	}
	return repos, !results.Search.PageInfo.HasNextPage, nil
}

// IterSearch calls Search repeatedly until all results are fetched, yielding each _unique_ repository
func IterSearch(
	ctx context.Context,
	client *githubv4.Client,
	query string,
) iter.Seq2[Repository, error] {
	return func(yield func(Repository, error) bool) {
		var since *time.Time
		uniq := make(map[string]struct{})
		for {
			repos, done, err := Search(ctx, client, query, since)
			if err != nil {
				// We hit secondary rate limit errors sometimes, just wait a bit
				if strings.Contains(err.Error(), "You have exceeded a secondary rate limit.") {
					log.Printf("sleeping: %s", err.Error())
					time.Sleep(10 * time.Second)
					continue // Retry
				}
				yield(Repository{}, err)
				return // Stop iteration on error
			}
			for _, repo := range repos {
				if _, ok := uniq[repo.NameWithOwner]; ok {
					continue // Skip duplicate entries
				}
				uniq[repo.NameWithOwner] = struct{}{}
				if !yield(repo, nil) {
					return
				}
			}
			// If we reached the end of the results, we're done!
			if done {
				return
			}
			// The next search should start from the last pushed at time of the last repository
			if len(repos) > 0 {
				since = &repos[len(repos)-1].PushedAt.Time
			}
		}
	}
}

func main() {
	ctx, cancel := signal.NotifyContext(context.Background(), os.Interrupt)
	defer cancel()

	if len(os.Args) != 2 {
		fmt.Fprintf(os.Stderr, "Usage: %s [repository search query]\n", filepath.Base(os.Args[0]))
		os.Exit(1)
	}

	transport, err := ghauth.Transport(ctx, nil)
	if err != nil {
		log.Fatalf("ghauth.Transport failed: %v", err)
	}
	client := githubv4.NewClient(&http.Client{
		Transport: transport,
		Timeout:   30 * time.Second,
	})

	writer := csv.NewWriter(os.Stdout)
	defer writer.Flush()
	for repo, err := range IterSearch(ctx, client, os.Args[1]) {
		if err != nil {
			log.Fatalf("Search failed: %v", err)
		}
		if err := writer.Write([]string{
			repo.NameWithOwner,
			strconv.Itoa(repo.StargazerCount),
			repo.CreatedAt.Time.Format(time.RFC3339),
			repo.PushedAt.Time.Format(time.RFC3339),
		}); err != nil {
			log.Fatalf("(*csv.Writer).Write failed: %v", err)
		}
	}
	if err := writer.Error(); err != nil {
		log.Fatalf("(*csv.Writer).Flush failed: %v", err)
	}
}

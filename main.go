package main

import (
	"context"
	"fmt"
	"log"
	"os"
	"os/signal"

	"github.com/shurcooL/githubv4"
	"golang.org/x/oauth2"
)

// https://docs.github.com/en/graphql/reference/objects#repository
type Repository struct {
	NameWithOwner  string
	StargazerCount int
	ForkCount      int
	DiskUsage      int
}

// Search performs a search of repositories matching the query.
func RepositorySearch(ctx context.Context, client *githubv4.Client, query string) ([]Repository, error) {
	// https://docs.github.com/en/graphql/reference/queries#search
	var q struct {
		Search struct {
			Nodes []struct {
				Repository Repository `graphql:"... on Repository"`
			}
			PageInfo struct {
				EndCursor   githubv4.String
				HasNextPage bool
			}
		} `graphql:"search(query: $query, type: REPOSITORY, first: 100, after: $cursor)"`
	}
	// https://docs.github.com/en/graphql/guides/using-pagination-in-the-graphql-api
	var cursor *githubv4.String
	var repos []Repository
	for {
		if err := client.Query(ctx, &q, map[string]any{
			"query":  githubv4.String(query),
			"cursor": cursor,
		}); err != nil {
			return nil, err
		}
		for _, node := range q.Search.Nodes {
			repos = append(repos, node.Repository)
		}
		if q.Search.PageInfo.HasNextPage {
			cursor = githubv4.NewString(q.Search.PageInfo.EndCursor)
		} else {
			return repos, nil
		}
	}
}

// Entry Point
func main() {
	ctx, cancel := signal.NotifyContext(context.Background(), os.Interrupt)
	defer cancel()

	// GraphQL client from GITHUB_TOKEN environment variable
	client := githubv4.NewClient(oauth2.NewClient(ctx, oauth2.StaticTokenSource(
		&oauth2.Token{AccessToken: os.Getenv("GITHUB_TOKEN")},
	)))

	// Parse the CLI args
	var field, query string
	switch len(os.Args) {
	case 3:
		query = os.Args[2] + " "
		fallthrough
	case 2:
		field = os.Args[1]
		switch field {
		default:
			log.Fatalf("Unsupported field: %q", field)
		case "stars", "forks", "size":
		}
	default:
		fmt.Fprintf(os.Stderr, "Usage: %s (stars|forks|size) [query]\n", os.Args[0])
		os.Exit(1)
	}

	// De-duplicate repos since we can't use the cursor forever
	var lastValue int
	uniq := make(map[string]struct{})
	for {
		// Sort the results by the highest value first
		query := query + "sort:" + field
		if lastValue == 0 {
			query += fmt.Sprintf(" %s:>0", field)
		} else {
			query += fmt.Sprintf(" %s:<=%d", field, lastValue)
		}
		// Run the query in batches of 1000 repos
		repos, err := RepositorySearch(ctx, client, query)
		if err != nil {
			log.Fatal(err)
		} else if len(repos) == 0 {
			break
		}
		// Print the requested field for each repo
		var value int
		for _, repo := range repos {
			switch field {
			case "stars":
				value = repo.StargazerCount
			case "forks":
				value = repo.ForkCount
			case "size":
				value = repo.DiskUsage
			}
			if _, ok := uniq[repo.NameWithOwner]; !ok {
				uniq[repo.NameWithOwner] = struct{}{}
				fmt.Printf("%s,%d\n", repo.NameWithOwner, value)
			}
		}
		// If we have the same value as the start of this batch, can't loop further
		if value == lastValue {
			break
		}
		lastValue = value
	}
}

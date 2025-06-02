package main

import (
	"context"
	"encoding/base64"
	"encoding/csv"
	"fmt"
	"log"
	"net/http"
	"os"
	"os/signal"
	"strconv"
	"strings"
	"time"

	ghauth "github.com/bored-engineer/github-auth-http-transport"
	"github.com/shurcooL/githubv4"
	"github.com/spf13/pflag"
)

// csvDateTime formats a githubv4.DateTime as a string for CSV output.
func csvDateTime(dt githubv4.DateTime) string {
	if dt.IsZero() {
		return ""
	}
	return dt.Time.Format(time.RFC3339)
}

// Repository is a struct that represents a GitHub repository.
type Repository struct {
	ArchivedAt     githubv4.DateTime
	CreatedAt      githubv4.DateTime
	DatabaseId     int64
	DiskUsage      int64
	ForkCount      int64
	NameWithOwner  string
	PushedAt       githubv4.DateTime
	StargazerCount int64
	UpdatedAt      githubv4.DateTime
}

// Search runs a GitHub search query using to retrieve a list of matching repositories.
func Search(
	ctx context.Context,
	client *githubv4.Client,
	query string,
) (repos []Repository, _ error) {
	// Loop but with overlapping offsets to ensure we don't miss any results
	uniq := make(map[int64]struct{})
	for offset := 0; offset < 1000; offset += 91 {
	Retry:
		var cursor *githubv4.String
		if offset > 0 {
			cursor = githubv4.NewString(githubv4.String(
				base64.StdEncoding.EncodeToString(
					[]byte(fmt.Sprintf("cursor:%d", offset)),
				),
			))
		}
		var results struct {
			Search struct {
				Nodes []struct {
					Repository Repository `graphql:"... on Repository"`
				}
				PageInfo struct {
					HasNextPage bool
				}
			} `graphql:"search(query: $query, type: REPOSITORY, first: 100, after: $cursor)"`
		}
		if err := client.Query(ctx, &results, map[string]any{
			"query":  githubv4.String(query),
			"cursor": cursor,
		}); err != nil {
			// We hit secondary rate limit errors sometimes, just wait a bit
			// We've also seen "something went wrong" before, retry those
			if strings.Contains(err.Error(), "You have exceeded a secondary rate limit.") || strings.Contains(err.Error(), "Something went wrong while executing your query.") || strings.Contains(err.Error(), "504 Gateway Timeout") {
				log.Printf("sleeping: %s", err.Error())
				time.Sleep(10 * time.Second)
				goto Retry
			}
			return nil, err
		}
		log.Printf("searching(%d): %s", offset, query)
		for _, node := range results.Search.Nodes {
			if _, ok := uniq[node.Repository.DatabaseId]; ok {
				continue // Skip duplicate entries
			}
			uniq[node.Repository.DatabaseId] = struct{}{}
			repos = append(repos, node.Repository)
		}
		if !results.Search.PageInfo.HasNextPage {
			break // No more pages, exit the loop early
		}
	}
	return repos, nil
}

func main() {
	ctx, cancel := signal.NotifyContext(context.Background(), os.Interrupt)
	defer cancel()

	query := pflag.StringP("query", "q", "", "GitHub search query")
	start := pflag.StringP("start", "s", "", "Start date for filtering repositories (RFC3339 format)")
	end := pflag.StringP("end", "e", "", "End date for filtering repositories (RFC3339 format)")
	pflag.Parse()
	if *query == "" || *start == "" || *end == "" {
		pflag.Usage()
		os.Exit(1)
	}

	startTime, err := time.ParseInLocation("2006-01-02", *start, time.UTC)
	if err != nil {
		log.Fatalf("time.ParseInLocation failed: %v", err)
	}
	endTime, err := time.ParseInLocation("2006-01-02", *end, time.UTC)
	if err != nil {
		log.Fatalf("time.ParseInLocation failed: %v", err)
	}
	if startTime.After(endTime) {
		log.Fatalf("start date %s is after end date %s", startTime, endTime)
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
	for day := startTime; !day.After(endTime); day = day.AddDate(0, 0, 1) {
		query := fmt.Sprintf("%s created:%sT00:00:00Z..%sT23:59:59Z", *query, day.Format("2006-01-02"), day.Format("2006-01-02"))
		repos, err := Search(ctx, client, query)
		if err != nil {
			log.Fatalf("Search failed: %v", err)
		}
		for _, repo := range repos {
			owner, name, _ := strings.Cut(repo.NameWithOwner, "/")
			if err := writer.Write([]string{
				owner,
				name,
				strconv.FormatInt(repo.DatabaseId, 10),
				strconv.FormatInt(repo.StargazerCount, 10),
				strconv.FormatInt(repo.ForkCount, 10),
				strconv.FormatInt(repo.DiskUsage, 10),
				csvDateTime(repo.CreatedAt),
				csvDateTime(repo.UpdatedAt),
				csvDateTime(repo.PushedAt),
				csvDateTime(repo.ArchivedAt),
			}); err != nil {
				log.Fatalf("(*csv.Writer).Write failed: %v", err)
			}
		}
	}
	if err := writer.Error(); err != nil {
		log.Fatalf("(*csv.Writer).Flush failed: %v", err)
	}
}

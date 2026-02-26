package main

import (
	"bytes"
	"context"
	"encoding/base64"
	"encoding/csv"
	"encoding/json"
	"fmt"
	"io"
	"log"
	"net/http"
	"os"
	"os/signal"
	"strconv"
	"strings"
	"sync"
	"time"

	ratelimit "github.com/bored-engineer/ratelimit-transport"
	oauth2githubapp "github.com/int128/oauth2-github-app"
	"github.com/spf13/pflag"
	"golang.org/x/oauth2"
	"golang.org/x/sync/errgroup"
)

// csvDateTime formats a *time.Time as a string for CSV output.
func csvDateTime(dt *time.Time) string {
	if dt == nil || dt.IsZero() {
		return ""
	}
	return dt.Format(time.RFC3339)
}

const url = "https://api.github.com/graphql"
const payloadSearch = `query($query: String!, $cursor: String) {
	search(query: $query, type: REPOSITORY, first: 100, after: $cursor) {
		repositoryCount
		pageInfo {
			hasNextPage
		}
		nodes {
			... on Repository {
				archivedAt
				createdAt
				databaseId
				diskUsage
				forkCount
				nameWithOwner
				pushedAt
				stargazerCount
				updatedAt
			}
		}
	}
}`

// Repository is a struct that represents a GitHub repository.
type Repository struct {
	ArchivedAt     *time.Time `json:"archivedAt"`
	CreatedAt      *time.Time `json:"createdAt"`
	DatabaseId     int64      `json:"databaseId"`
	DiskUsage      int64      `json:"diskUsage"`
	ForkCount      int64      `json:"forkCount"`
	NameWithOwner  string     `json:"nameWithOwner"`
	PushedAt       *time.Time `json:"pushedAt"`
	StargazerCount int64      `json:"stargazerCount"`
	UpdatedAt      *time.Time `json:"updatedAt"`
}

// Search runs a GitHub search query for a single page of results
func Search(
	ctx context.Context,
	client *http.Client,
	query string,
	cursor string,
) (int64, []Repository, bool, error) {
	vars := map[string]any{
		"query": query,
	}
	if cursor != "" {
		vars["cursor"] = cursor
	}
	reqBytes, err := json.Marshal(struct {
		Query     string         `json:"query"`
		Variables map[string]any `json:"variables"`
	}{
		Query:     payloadSearch,
		Variables: vars,
	})
	if err != nil {
		return 0, nil, false, fmt.Errorf("json.Marshal failed: %w", err)
	}

	req, err := http.NewRequestWithContext(ctx, http.MethodPost, url, bytes.NewReader(reqBytes))
	if err != nil {
		return 0, nil, false, fmt.Errorf("http.NewRequestWithContext failed: %w", err)
	}
	req.Header.Set("Content-Type", "application/json")
	req.Header.Set("X-GitHub-Api-Version", "2022-11-28")
	req.Header.Set("X-Github-Next-Global-ID", "1")

	resp, err := client.Do(req)
	if err != nil {
		return 0, nil, false, fmt.Errorf("(*http.Client).Do failed: %w", err)
	}
	defer resp.Body.Close()
	body, err := io.ReadAll(resp.Body)
	if err != nil {
		return 0, nil, false, fmt.Errorf("(*http.Response).Body.Read failed: %w", err)
	}
	if err := resp.Body.Close(); err != nil {
		return 0, nil, false, fmt.Errorf("(*http.Response).Body.Close failed: %w", err)
	}

	if resp.StatusCode != http.StatusOK {
		return 0, nil, false, fmt.Errorf("(*http.Client).Do failed with %s (%d): %s", resp.Status, resp.StatusCode, string(body))
	}
	var decoded struct {
		Data struct {
			Search struct {
				RepositoryCount int64 `json:"repositoryCount"`
				PageInfo        struct {
					HasNextPage bool `json:"hasNextPage"`
				} `json:"pageInfo"`
				Nodes []Repository `json:"nodes"`
			} `json:"search"`
		} `json:"data"`
	}
	if err := json.Unmarshal(body, &decoded); err != nil {
		return 0, nil, false, fmt.Errorf("json.Unmarshal failed: %w", err)
	}
	return decoded.Data.Search.RepositoryCount, decoded.Data.Search.Nodes, decoded.Data.Search.PageInfo.HasNextPage, nil
}

// SearchPaginated loops over each page of results including retrying on errors
func SearchPaginated(
	ctx context.Context,
	client *http.Client,
	query string,
) (int64, []Repository, error) {
	// Deduplicate the results by databaseId
	uniq := make(map[int64]struct{})
	var repositories []Repository
	// Loop but with overlapping offsets to ensure we don't miss any results
	for offset := 0; offset < 1000; offset += 91 {
	Retry:
		var cursor string
		if offset > 0 {
			cursor = base64.StdEncoding.EncodeToString(
				fmt.Appendf(nil, "cursor:%d", offset),
			)
		}
		repositoryCount, page, hasNextPage, err := Search(ctx, client, query, cursor)
		if err != nil {
			// We hit secondary rate limit errors sometimes, just wait a bit
			// We've also seen "something went wrong" before, retry those
			if strings.Contains(err.Error(), "You have exceeded a secondary rate limit") ||
				strings.Contains(err.Error(), "Something went wrong while executing your query") ||
				strings.Contains(err.Error(), "403 Forbidden") ||
				strings.Contains(err.Error(), "502 Bad Gateway") ||
				strings.Contains(err.Error(), "504 Gateway Timeout") {
				time.Sleep(10 * time.Second)
				goto Retry
			}
			return 0, nil, fmt.Errorf("Search for %q at offset %d failed: %w", query, offset, err)
		}
		// If we've got more than 1000 repositories, bail, let the caller split the search space
		if repositoryCount >= 1000 {
			return repositoryCount, nil, nil
		}
		for _, repository := range page {
			if _, ok := uniq[repository.DatabaseId]; ok {
				continue // Skip duplicate entries
			}
			uniq[repository.DatabaseId] = struct{}{}
			repositories = append(repositories, repository)
		}
		if !hasNextPage {
			break
		}
	}
	return int64(len(repositories)), repositories, nil
}

// SearchChunks splits the search space into chunks of <1000 repositories
func SearchChunks(
	ctx context.Context,
	client *http.Client,
	query string,
	start time.Time,
	end time.Time,
) ([]Repository, error) {
	// In the ideal case, we'll get all the repositories in one go
	queryChunk := query + " created:" + start.Format(time.RFC3339) + ".." + end.Format(time.RFC3339)
	repositoryCount, repositories, err := SearchPaginated(ctx, client, queryChunk)
	if err != nil {
		return nil, err
	}
	if repositoryCount < 1000 {
		return repositories, nil
	}
	// Assume that the results are evenly distributed, split into the number of chunks based on roughly 1000 results per chunk, rounded up
	repositories = nil
	chunks := (repositoryCount / 1000) + 1
	interval := (end.Sub(start) / time.Duration(chunks)).Round(time.Second)
	for intervalStart := start; intervalStart.Before(end); intervalStart = intervalStart.Add(interval).Add(time.Second) {
		intervalEnd := intervalStart.Add(interval)
		if intervalEnd.After(end) {
			intervalEnd = end
		}
		chunk, err := SearchChunks(ctx, client, query, intervalStart, intervalEnd)
		if err != nil {
			return nil, err
		}
		repositories = append(repositories, chunk...)
	}
	return repositories, nil
}

func main() {
	// GitHub was founded in 2007, and default the end as tomorrow
	nowUTC := time.Now().UTC()
	defaultStart := time.Date(2007, 1, 1, 0, 0, 0, 0, time.UTC)
	defaultEnd := time.Date(nowUTC.Year(), nowUTC.Month(), nowUTC.Day(), 0, 0, 0, 0, time.UTC)

	query := pflag.StringP("query", "q", "", "GitHub search query")
	start := pflag.TimeP("start", "s", defaultStart, []string{"2006-01-02"}, "Start date for filtering repositories")
	end := pflag.TimeP("end", "e", defaultEnd, []string{"2006-01-02"}, "End date for filtering repositories")
	rate := pflag.IntP("rate", "r", 4950, "Rate limit for making requests per hour")
	pflag.Parse()
	if *query == "" {
		pflag.Usage()
		os.Exit(1)
	}

	ctx, cancel := signal.NotifyContext(context.Background(), os.Interrupt)
	defer cancel()
	wg, ctx := errgroup.WithContext(ctx)

	days := make(chan time.Time)
	go func() {
		defer close(days)
		for day := *start; !day.After(*end); day = day.AddDate(0, 0, 1) {
			days <- day
		}
	}()

	var writerMu sync.Mutex
	writer := csv.NewWriter(os.Stdout)
	writer.Write([]string{
		"owner",
		"name",
		"id",
		"stars",
		"forks",
		"size",
		"created_at",
		"updated_at",
		"pushed_at",
		"archived_at",
	})

	// Build a list of bearer tokens to utilize for parallel searching
	var tokenSources []oauth2.TokenSource
	for idx := 1; ; idx++ {
		appID := os.Getenv(fmt.Sprintf("GH_APP_ID_%d", idx))
		installationID := os.Getenv(fmt.Sprintf("GH_APP_INSTALLATION_ID_%d", idx))
		privateKeyRaw := os.Getenv(fmt.Sprintf("GH_APP_PRIVATE_KEY_%d", idx))
		if appID == "" || installationID == "" || privateKeyRaw == "" {
			break
		}
		privateKey, err := oauth2githubapp.ParsePrivateKey([]byte(privateKeyRaw))
		if err != nil {
			log.Fatalf("oauth2githubapp.ParsePrivateKey for GH_APP_PRIVATE_KEY_%d failed: %v", idx, err)
		}
		cfg := &oauth2githubapp.Config{
			PrivateKey:     privateKey,
			AppID:          appID,
			InstallationID: installationID,
		}
		tokenSources = append(tokenSources, cfg.TokenSource(ctx))
	}
	if len(tokenSources) == 0 {
		tokenSources = append(tokenSources, oauth2.StaticTokenSource(&oauth2.Token{
			AccessToken: os.Getenv("GH_TOKEN"),
		}))
	}

	for _, tokenSource := range tokenSources {
		client := &http.Client{
			Transport: ratelimit.New(&oauth2.Transport{
				Base:   http.DefaultTransport,
				Source: tokenSource,
			}, *rate, ratelimit.Per(time.Hour)),
			Timeout: 10 * time.Second,
		}
		wg.Go(func() error {
			for {
				select {
				case <-ctx.Done():
					return ctx.Err()
				case start, ok := <-days:
					if !ok {
						// No more days to process
						return nil
					}
					// End with the very end of that day
					end := time.Date(start.Year(), start.Month(), start.Day(), 23, 59, 59, 0, start.Location())
					repositories, err := SearchChunks(ctx, client, *query, start, end)
					if err != nil {
						return fmt.Errorf("SearchChunks for %s failed: %w", start.Format("2006-01-02"), err)
					}
					log.Printf("gathered %d repositories for %s", len(repositories), start.Format("2006-01-02"))
					for _, repo := range repositories {
						owner, name, _ := strings.Cut(repo.NameWithOwner, "/")
						writerMu.Lock()
						err := writer.Write([]string{
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
						})
						writerMu.Unlock()
						if err != nil {
							return fmt.Errorf("(*csv.Writer).Write failed: %w", err)
						}
					}
				}
			}
		})
	}
	if err := wg.Wait(); err != nil {
		log.Fatal(err)
	}

	writer.Flush()
	if err := writer.Error(); err != nil {
		log.Fatalf("(*csv.Writer).Flush failed: %v", err)
	}
}

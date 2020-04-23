package main

import (
	"context"
	"errors"
	"fmt"
	"io"
	"net/http"
	"os"
	"strings"
	"time"

	"github.com/dustin/go-humanize"
	"github.com/spf13/pflag"
	"github.com/spf13/viper"
	"golang.org/x/oauth2"

	rest "github.com/google/go-github/v31/github"
	graphql "github.com/shurcooL/githubv4"
)

var (
	// options
	token        string
	organization string
	repos        []string
	lock         bool
	help         bool
	cfg          string

	// -----

	ctx           = context.Background()
	httpClient    *http.Client
	restClient    *rest.Client
	graphqlClient *graphql.Client

	// -----

	query struct {
		Organization struct {
			Repositories struct {
				PageInfo struct {
					EndCursor   graphql.String
					HasNextPage bool
				}
				Nodes []Repository
			} `graphql:"repositories(first: 100, after: $page)"`
		} `graphql:"organization(login: $login)"`
	}
)

// Repository unexported
type Repository struct {
	Name string
}

func init() {
	// flags
	pflag.BoolVarP(&help, "help", "h", false, "Print this help.")
	pflag.StringVarP(&cfg, "config", "c", "", "Path to config file. Default: .ghec-backup in current directory")
	pflag.StringVarP(&organization, "organization", "o", "", "Organization on github.com to backup.")
	pflag.StringSliceVarP(&repos, "repository", "r", make([]string, 0), "Repository to backup, can be provided multiple times. Default: organization repositories")
	pflag.BoolVarP(&lock, "lock", "l", false, "Lock repositories while backing up. Default: false")
	pflag.Parse()

	// read config
	viper.SetConfigName(".ghec-backup")
	viper.SetConfigType("yml")

	if cfg != "" {
		viper.AddConfigPath(cfg)
	} else {
		viper.AddConfigPath(".")
	}

	if err := viper.ReadInConfig(); err != nil && cfg != "" {
		printHelpOnError(
			fmt.Sprintf("config file .ghec-backup not found in %s", cfg),
		)
	}
	viper.BindPFlags(pflag.CommandLine)

	// assign values
	help = viper.GetBool("help")
	token = viper.GetString("token")
	organization = viper.GetString("organization")
	repos = viper.GetStringSlice("repository")
	lock = viper.GetBool("lock")

	// validate
	validateFlags()

	// -----

	src := oauth2.StaticTokenSource(&oauth2.Token{AccessToken: token})
	httpClient = oauth2.NewClient(ctx, src)

	graphqlClient = graphql.NewClient(httpClient)
	restClient = rest.NewClient(httpClient)
}

func main() {
	now := time.Now()

	if err := parseRepos(); err != nil {
		errorAndExit(err)
	}

	m, _, err := restClient.Migrations.StartMigration(
		ctx,
		organization,
		repos,
		&rest.MigrationOptions{
			LockRepositories:   lock,
			ExcludeAttachments: true,
		},
	)

	if err != nil {
		errorAndExit(err)
	}

	id := m.GetID()

	fmt.Printf("Creating backup archive (%v) ", id)
	for {
		exported, err := getMigrationStatus(id)

		if err != nil {
			errorAndExit(err)
		}

		if exported {
			break
		}

		// sleep 3.6s to not hit (abuse) rate limit
		time.Sleep(3600 * time.Millisecond)
	}
	fmt.Printf(" complete\n")

	// download backup archive
	url, _ := restClient.Migrations.MigrationArchiveURL(ctx, organization, id)
	e := DownloadFile(
		fmt.Sprintf("backup.%v.tar.gz", now.Unix()),
		url,
	)

	if e != nil {
		errorAndExit(e)
	}

	// unlock repositories if they were locked for backup
	if lock {
		for _, r := range repos {
			restClient.Migrations.UnlockRepo(ctx, organization, id, r)
			fmt.Printf("%v/%v unlocked\n", organization, r)
		}
	}

	// delete archive
	fmt.Printf("Cleaning up (%v)", id)
	restClient.Migrations.DeleteMigration(
		ctx,
		organization,
		id,
	)
	fmt.Printf(" complete\n")
}

// helpers ---------------------------------------------------------------------

func parseRepos() (err error) {
	if len(repos) == 0 {
		variables := map[string]interface{}{
			"login": graphql.String(organization),
			"page":  (*graphql.String)(nil),
		}

		var repositories []Repository

		for {
			err = graphqlClient.Query(ctx, &query, variables)

			repositories = append(repositories, query.Organization.Repositories.Nodes...)

			// break on last page
			if !query.Organization.Repositories.PageInfo.HasNextPage {
				break
			}

			variables["page"] = graphql.NewString(query.Organization.Repositories.PageInfo.EndCursor)
		}

		for _, repo := range repositories {
			repos = append(repos, repo.Name)
		}
	}

	return
}

func getMigrationStatus(id int64) (exported bool, err error) {
	status, _, err := restClient.Migrations.MigrationStatus(
		ctx,
		organization,
		id,
	)

	if err != nil {
		return false, err
	}

	s := status.GetState()

	fmt.Printf(".")

	if s == "failed" {
		return false, err
	}

	return s == "exported", nil
}

// WriteCounter counts the number of bytes written to it. It implements to the io.Writer interface
// and we can pass this into io.TeeReader() which will report progress on each write cycle.
type WriteCounter struct {
	Total uint64
}

func (wc *WriteCounter) Write(p []byte) (int, error) {
	n := len(p)
	wc.Total += uint64(n)
	wc.PrintProgress()
	return n, nil
}

// PrintProgress unexported
func (wc WriteCounter) PrintProgress() {
	// Clear the line by using a character return to go back to the start and remove
	// the remaining characters by filling it with spaces
	fmt.Printf("\r%s", strings.Repeat(" ", 35))

	// Return again and print current status of download
	// We use the humanize package to print the bytes in a meaningful way (e.g. 10 MB)
	fmt.Printf("\rDownloading %s", humanize.Bytes(wc.Total))
}

// DownloadFile will download a url to a local file. It's efficient because it will
// write as it downloads and not load the whole file into memory. We pass an io.TeeReader
// into Copy() to report progress on the download.
func DownloadFile(filepath string, url string) error {

	// Create the file, but give it a tmp file extension, this means we won't overwrite a
	// file until it's downloaded, but we'll remove the tmp extension once downloaded.
	out, err := os.Create(filepath + ".tmp")
	if err != nil {
		return err
	}

	// Get the data
	resp, err := http.Get(url)
	if err != nil {
		out.Close()
		return err
	}
	defer resp.Body.Close()

	// Create our progress reporter and pass it to be used alongside our writer
	counter := &WriteCounter{}
	if _, err = io.Copy(out, io.TeeReader(resp.Body, counter)); err != nil {
		out.Close()
		return err
	}

	// The progress use the same line so print a new line once it's finished downloading
	fmt.Print("\n")

	// Close the file without defer so it can happen before Rename()
	out.Close()

	if err = os.Rename(filepath+".tmp", filepath); err != nil {
		return err
	}
	return nil
}

func validateFlags() {
	if help {
		printHelp()
		os.Exit(0)
	}

	if token == "" {
		printHelpOnError("token missing")
	}

	if organization == "" {
		printHelpOnError("organization is required")
	}
}

func printHelp() {
	fmt.Println(`USAGE:
  ghec-backup [OPTIONS]

OPTIONS:`)
	pflag.PrintDefaults()
	fmt.Println(`
EXAMPLE:
  $ ghec-backup`)
	fmt.Println()
}

func printHelpOnError(s string) {
	printHelp()
	errorAndExit(errors.New(s))
}

func errorAndExit(err error) {
	fmt.Fprintf(os.Stderr, "error: %s\n", err)
	os.Exit(2)
}

package main

import (
	"bufio"
	"crypto/tls"
	"errors"
	"flag"
	"fmt"
	"log"
	"net/http"
	"net/url"
	"os"
	"regexp"
	"strings"
	"sync"

	"github.com/gocolly/colly/v2"
)

var headers map[string]string

type input struct {
	Type  string
	Name  string
	Value string
}

type form struct {
	URL    string
	Method string
	Inputs []input
}

// Thread safe map
var sm sync.Map
var smform sync.Map

func main() {
	threads := flag.Int("t", 8, "Number of threads to utilise.")
	depth := flag.Int("d", 2, "Depth to crawl.")
	insecure := flag.Bool("insecure", false, "Disable TLS verification.")
	subsInScope := flag.Bool("subs", false, "Include subdomains for crawling.")
	showSource := flag.Bool("s", false, "Show the source of URL based on where it was found (href, form, script, etc.)")
	rawHeaders := flag.String(("h"), "", "Custom headers separated by two semi-colons. E.g. -h \"Cookie: foo=bar;;Referer: http://example.com/\" ")
	unique := flag.Bool(("u"), false, "Show only unique urls")

	flag.Parse()

	// Convert the headers input to a usable map (or die trying)
	err := parseHeaders(*rawHeaders)
	if err != nil {
		fmt.Fprintln(os.Stderr, "Error parsing headers:", err)
		os.Exit(1)
	}

	// Check for stdin input
	stat, _ := os.Stdin.Stat()
	if (stat.Mode() & os.ModeCharDevice) != 0 {
		fmt.Fprintln(os.Stderr, "No urls detected. Hint: cat urls.txt | hakrawler")
		os.Exit(1)
	}

	results := make(chan string, *threads)
	formchan := make(chan string, *threads)
	go func() {
		// get each line of stdin, push it to the work channel
		s := bufio.NewScanner(os.Stdin)
		for s.Scan() {
			url := s.Text()
			hostname, err := extractHostname(url)
			if err != nil {
				log.Println("Error parsing URL:", err)
				return
			}

			allowed_domains := []string{hostname}
			// if "Host" header is set, append it to allowed domains
			if headers != nil {
				if val, ok := headers["Host"]; ok {
					allowed_domains = append(allowed_domains, val)
				}
			}

			// Instantiate default collector
			c := colly.NewCollector(
				// default user agent header
				colly.UserAgent("Mozilla/5.0 (X11; Linux x86_64; rv:78.0) Gecko/20100101 Firefox/78.0"),
				// set custom headers
				colly.Headers(headers),
				// limit crawling to the domain of the specified URL
				colly.AllowedDomains(allowed_domains...),
				// set MaxDepth to the specified depth
				colly.MaxDepth(*depth),
				// specify Async for threading
				colly.Async(true),
			)

			// if -subs is present, use regex to filter out subdomains in scope.
			if *subsInScope {
				c.AllowedDomains = nil
				c.URLFilters = []*regexp.Regexp{regexp.MustCompile(".*(\\.|\\/\\/)" + strings.ReplaceAll(hostname, ".", "\\.") + "((#|\\/|\\?).*)?")}
			}

			// Set parallelism
			c.Limit(&colly.LimitRule{DomainGlob: "*", Parallelism: *threads})

			// Print every href found, and visit it
			c.OnHTML("a[href]", func(e *colly.HTMLElement) {
				link := e.Attr("href")
				printResult(link, "href", *showSource, results, e)
				e.Request.Visit(link)
			})

			// find and print all the JavaScript files
			c.OnHTML("script[src]", func(e *colly.HTMLElement) {
				printResult(e.Attr("src"), "script", *showSource, results, e)
			})

			// find and print all the form action URLs
			c.OnHTML("form[action]", func(e *colly.HTMLElement) {
				printResult(e.Attr("action"), "form", *showSource, results, e)
			})

			c.OnHTML("form", func(e *colly.HTMLElement) {
				action := e.Request.AbsoluteURL(e.Attr("action"))
				method := e.Attr("method")

				var inputs []input
				e.ForEach("input", func(_ int, e *colly.HTMLElement) {
					inputs = append(inputs, input{
						Type:  e.Attr("type"),
						Name:  e.Attr("name"),
						Value: e.Attr("value"),
					})
				})
				e.ForEach("textarea", func(_ int, e *colly.HTMLElement) {
					inputs = append(inputs, input{
						Type:  "text",
						Name:  e.Attr("name"),
						Value: e.Attr("value"),
					})
				})

				f := form{
					URL:    action,
					Method: method,
					Inputs: inputs,
				}
				testReflection(f)
				printForm(f, formchan)
			})

			// add the custom headers
			if headers != nil {
				c.OnRequest(func(r *colly.Request) {
					for header, value := range headers {
						r.Headers.Set(header, value)
					}
				})
			}

			// Skip TLS verification if -insecure flag is present
			c.WithTransport(&http.Transport{
				TLSClientConfig: &tls.Config{InsecureSkipVerify: *insecure},
			})

			// Start scraping
			c.Visit(url)
			// Wait until threads are finished
			c.Wait()

		}
		if err := s.Err(); err != nil {
			fmt.Fprintln(os.Stderr, "reading standard input:", err)
		}
		close(results)
		close(formchan)
	}()

	w := bufio.NewWriter(os.Stdout)
	if *unique {
		for res := range results {
			if isUnique(res) {
				fmt.Fprintln(w, res)
			}
		}
	}
	for res := range results {
		fmt.Fprintln(w, res)
	}
	if *unique {
		for res := range formchan {
			if isUnique(res) {
				fmt.Fprintln(w, res)
			}
		}
	}
	for res := range formchan {
		fmt.Fprintln(w, res)
	}
	w.Flush()

	/*
		var forms []form

		if *unique {
			for res := range formchan {
				if isUniqueForm(res) {
					fmt.Println(res.URL, res.Method)
					forms = append(forms, res)
				}
			}
		}
		for res := range formchan {
			forms = append(forms, res)
		}
	*/
}

// parseHeaders does validation of headers input and saves it to a formatted map.
func parseHeaders(rawHeaders string) error {
	if rawHeaders != "" {
		if !strings.Contains(rawHeaders, ":") {
			return errors.New("headers flag not formatted properly (no colon to separate header and value)")
		}

		headers = make(map[string]string)
		rawHeaders := strings.Split(rawHeaders, ";;")
		for _, header := range rawHeaders {
			var parts []string
			if strings.Contains(header, ": ") {
				parts = strings.SplitN(header, ": ", 2)
			} else if strings.Contains(header, ":") {
				parts = strings.SplitN(header, ":", 2)
			} else {
				continue
			}
			headers[strings.TrimSpace(parts[0])] = strings.TrimSpace(parts[1])
		}
	}
	return nil
}

// extractHostname() extracts the hostname from a URL and returns it
func extractHostname(urlString string) (string, error) {
	u, err := url.Parse(urlString)
	if err != nil {
		return "", err
	}
	return u.Hostname(), nil
}

// print result constructs output lines and sends them to the results chan
func printResult(link string, sourceName string, showSource bool, results chan string, e *colly.HTMLElement) {
	result := e.Request.AbsoluteURL(link)
	if result != "" {
		if showSource {
			result = "[" + sourceName + "] " + result
		}
		results <- result
	}
}

// print form constructs output lines and sends them to the form chan
func printForm(f form, formchan chan string) {
	result := fmt.Sprintf("%s %s %s", f.Method, f.URL, "Inputs:")
	for i := 0; i < len(f.Inputs); i++ {
		result = fmt.Sprintf("%s %s %s", result, f.Inputs[i].Type, f.Inputs[i].Name)
	}
	if result != "" {
		formchan <- result
	}
}

// returns whether the supplied form object is unique or not
func isUniqueForm(f form) bool {
	hashable := fmt.Sprintf("%s%s", f.Method, f.URL)
	_, present := smform.Load(hashable)
	if present {
		return false
	}
	smform.Store(hashable, true)
	return true
}

// returns whether the supplied url is unique or not
func isUnique(url string) bool {
	_, present := sm.Load(url)
	if present {
		return false
	}
	sm.Store(url, true)
	return true
}

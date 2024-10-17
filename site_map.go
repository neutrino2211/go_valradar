package main

import (
	"io"
	"net/http"
	"net/url"
	"strings"
	"sync"
	"time"

	"golang.org/x/net/html"

	"github.com/briandowns/spinner"
	"github.com/neutrino2211/go-result"
)

type WebResourceType = uint8

const (
	PAGE_RESOURCE   WebResourceType = 0
	SCRIPT_RESOURCE WebResourceType = 1
)

type WebResource struct {
	url           string
	fetched       bool
	content       string
	resource_type WebResourceType
}

type SiteMap struct {
	url         string
	mutex       *sync.Mutex
	domain      string
	spinner     *spinner.Spinner
	fetcherFunc func(string) string
	resources   map[string]*WebResource
}

func (sm *SiteMap) setResource(r *WebResource) {
	sm.mutex.Lock()
	if _, ok := sm.resources[r.url]; !ok {
		sm.resources[r.url] = r
	}
	sm.mutex.Unlock()
}

func (sm *SiteMap) getFetcher() func(string) string {
	if sm.fetcherFunc != nil {
		return sm.fetcherFunc
	}

	return defaultFetcher
}

func defaultFetcher(url string) string {
	res := result.SomePair(http.DefaultClient.Get(url)).Expect("failed to GET " + url)
	val := result.SomePair(io.ReadAll(res.Body)).Expect("failed to read body for " + url)

	return string(val)
}

func NewSiteMap(u string) *SiteMap {
	parsedUrl := result.SomePair(url.Parse(u)).Expect("unable to parse the url " + u)
	domain := parsedUrl.Hostname()
	spinner := spinner.New(spinner.CharSets[7], 250*time.Millisecond)
	spinner.FinalMSG = "🕸️ Built map of " + u + "\n"
	spinner.Start()

	return &SiteMap{
		url:       u,
		mutex:     &sync.Mutex{},
		domain:    domain,
		spinner:   spinner,
		resources: map[string]*WebResource{},
	}
}

type CCR struct {
	size      int
	capacity  *int
	semaphore chan struct{}
	mutex     *sync.Mutex
}

func (ccr *CCR) limited(job func()) {
	ccr.semaphore <- struct{}{} // acquire
	ccr.mutex.Lock()
	*ccr.capacity -= 1
	ccr.mutex.Unlock()
	job()           // a job
	<-ccr.semaphore // release
	ccr.mutex.Lock()
	*ccr.capacity += 1
	ccr.mutex.Unlock()
}

func (ccr *CCR) start(job func()) {
	go ccr.limited(job)
}

func (ccr *CCR) wait() {
	time.Sleep(1 * time.Second)
	for *ccr.capacity < ccr.size {
		time.Sleep(100 * time.Millisecond)
		// println(*ccr.capacity)
	}
}

func NewCCR(concurrency int) *CCR {
	return &CCR{
		size:      concurrency,
		mutex:     &sync.Mutex{},
		capacity:  &concurrency,
		semaphore: make(chan struct{}, concurrency),
	}
}

func processNode(ccr *CCR, sm *SiteMap, r *[]*WebResource, u string, n *html.Node) {
	parsedUrl := result.SomePair(url.Parse(u)).Expect("unable to parse the url " + u)
	domain := parsedUrl.Hostname()

	fetchHtml := sm.getFetcher()

	switch n.Data {
	case "a":
	case "link":
		for _, attr := range n.Attr {
			if attr.Key == "href" && len(attr.Val) > 1 {
				if attr.Val[0] == '/' && attr.Val[1] != '/' {
					attr.Val = parsedUrl.Scheme + "://" + domain + attr.Val
				} else if attr.Val[0] == '/' && attr.Val[1] == '/' {
					attr.Val = parsedUrl.Scheme + ":" + attr.Val
				} else if attr.Val[0] == '#' || attr.Val[0:4] == "http" {
					continue
				}

				content := result.Try(func() string {
					sm.spinner.Prefix = " ⏳ "
					sm.spinner.Suffix = " Fetching: " + attr.Val
					val := fetchHtml(attr.Val)
					sm.spinner.Prefix = " ✅ "
					sm.spinner.Suffix = " Done: " + attr.Val

					return string(val)
				}).Or("")

				*r = append(*r, &WebResource{
					url:           attr.Val,
					content:       content,
					resource_type: PAGE_RESOURCE,
					fetched:       false,
				})
				break
			}
		}

	case "script":
		for _, attr := range n.Attr {
			if attr.Key == "src" {
				if attr.Val[0] == '/' {
					attr.Val = parsedUrl.Scheme + "://" + domain + attr.Val
				} else if attr.Val[0] == '#' || attr.Val[0:4] == "http" {
					continue
				}

				content := result.Try(func() string {
					sm.spinner.Prefix = " ⏳ "
					sm.spinner.Suffix = " Fetching: " + attr.Val
					val := fetchHtml(attr.Val)
					sm.spinner.Prefix = " ✅ "
					sm.spinner.Suffix = " Done: " + attr.Val

					return string(val)
				}).Or("")

				*r = append(*r, &WebResource{
					url:           attr.Val,
					content:       content,
					resource_type: SCRIPT_RESOURCE,
					fetched:       true,
				})
				break
			}
		}
	}

	// Traverse child nodes
	for c := n.FirstChild; c != nil; c = c.NextSibling {
		ccr.start(func() {
			processNode(ccr, sm, r, u, c)
		})
	}
}

func processAllLinks(ccr *CCR, sm *SiteMap, r *[]*WebResource, url string, n *html.Node) {
	// traverse the child nodes
	for c := n.FirstChild; c != nil; c = c.NextSibling {
		ccr.start(func() {
			processNode(ccr, sm, r, url, c)
		})
	}

	ccr.wait()
}

func getLinksAndContentFromUrl(ccr *CCR, sm *SiteMap, rs *[]*WebResource, url string) string {
	res := sm.getFetcher()(url)
	node := result.SomePair(html.Parse(strings.NewReader(res))).Expect("failed to parse html for " + url)

	processAllLinks(ccr, sm, rs, url, node)

	htmlString := res

	return string(htmlString)
}

func BuildSiteMap(ccr *CCR, sm *SiteMap, url string, depth int, maxDepth int) {
	sm.mutex.Lock()
	r := sm.resources[url]
	sm.mutex.Unlock()

	if depth == maxDepth || (r != nil && r.fetched) {
		return
	}

	sm.spinner.Prefix = " 🔨 "
	sm.spinner.Suffix = " Building: " + url
	resources := []*WebResource{}
	content := getLinksAndContentFromUrl(ccr, sm, &resources, url)

	sm.setResource(&WebResource{
		url:           url,
		content:       content,
		resource_type: PAGE_RESOURCE,
		fetched:       false,
	})

	for _, r := range resources {
		sm.setResource(r)
		if r.resource_type == PAGE_RESOURCE && r.url[0:4] == "http" {
			ccr.mutex.Lock()
			shouldDelay := *ccr.capacity < 2
			ccr.mutex.Unlock()

			if shouldDelay {
				time.Sleep(500 * time.Millisecond)
			}

			BuildSiteMap(ccr, sm, r.url, depth+1, maxDepth)

			r.fetched = true
		}
	}
}

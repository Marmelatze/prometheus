// Copyright 2013 The Prometheus Authors
// Licensed under the Apache License, Version 2.0 (the "License");
// you may not use this file except in compliance with the License.
// You may obtain a copy of the License at
//
// http://www.apache.org/licenses/LICENSE-2.0
//
// Unless required by applicable law or agreed to in writing, software
// distributed under the License is distributed on an "AS IS" BASIS,
// WITHOUT WARRANTIES OR CONDITIONS OF ANY KIND, either express or implied.
// See the License for the specific language governing permissions and
// limitations under the License.

package web

import (
	"fmt"
	"io"
	"io/ioutil"
	"net/http"
	"net/url"
	"os"
	"path/filepath"
	"sort"
	"strings"
	"sync"
	"time"

	template_std "html/template"
	pprof_runtime "runtime/pprof"

	clientmodel "github.com/prometheus/client_golang/model"
	"github.com/prometheus/client_golang/prometheus"
	"github.com/prometheus/log"

	"github.com/prometheus/prometheus/config"
	"github.com/prometheus/prometheus/promql"
	"github.com/prometheus/prometheus/retrieval"
	"github.com/prometheus/prometheus/rules"
	"github.com/prometheus/prometheus/storage/local"
	"github.com/prometheus/prometheus/template"
	"github.com/prometheus/prometheus/util/route"
	"github.com/prometheus/prometheus/web/api/legacy"
	"github.com/prometheus/prometheus/web/api/v1"
	"github.com/prometheus/prometheus/web/blob"
)

var localhostRepresentations = []string{"127.0.0.1", "localhost"}

// Handler serves various HTTP endpoints of the Prometheus server
type Handler struct {
	ruleManager *rules.Manager
	queryEngine *promql.Engine

	apiV1     *v1.API
	apiLegacy *legacy.API

	router  *route.Router
	quitCh  chan struct{}
	options *Options
	status  *PrometheusStatus

	muAlerts sync.Mutex
}

// PrometheusStatus contains various information about the status
// of the running Prometheus process.
type PrometheusStatus struct {
	Birth     time.Time
	BuildInfo map[string]string
	Flags     map[string]string
	Config    string

	// A function that returns the current scrape targets pooled
	// by their job name.
	TargetPools func() map[string][]*retrieval.Target
	// A function that returns all loaded rules.
	Rules func() []rules.Rule

	mu sync.RWMutex
}

// ApplyConfig updates the status state as the new config requires.
// Returns true on success.
func (s *PrometheusStatus) ApplyConfig(conf *config.Config) bool {
	s.mu.Lock()
	s.Config = conf.String()
	s.mu.Unlock()
	return true
}

// Options for the web Handler.
type Options struct {
	PathPrefix           string
	ListenAddress        string
	Hostname             string
	MetricsPath          string
	UseLocalAssets       bool
	UserAssetsPath       string
	ConsoleTemplatesPath string
	ConsoleLibrariesPath string
	EnableQuit           bool
}

// New initializes a new web Handler.
func New(st local.Storage, qe *promql.Engine, rm *rules.Manager, status *PrometheusStatus, o *Options) *Handler {
	router := route.New()

	h := &Handler{
		router:  router,
		quitCh:  make(chan struct{}),
		options: o,
		status:  status,

		ruleManager: rm,
		queryEngine: qe,

		apiV1: &v1.API{
			QueryEngine: qe,
			Storage:     st,
		},
		apiLegacy: &legacy.API{
			QueryEngine: qe,
			Storage:     st,
			Now:         clientmodel.Now,
		},
	}

	if o.PathPrefix != "" {
		// If the prefix is missing for the root path, prepend it.
		router.Get("/", func(w http.ResponseWriter, r *http.Request) {
			http.Redirect(w, r, o.PathPrefix, http.StatusFound)
		})
		router = router.WithPrefix(o.PathPrefix)
	}

	instrf := prometheus.InstrumentHandlerFunc
	instrh := prometheus.InstrumentHandler

	router.Get("/", instrf("status", h.statush))
	router.Get("/alerts", instrf("alerts", h.alerts))
	router.Get("/graph", instrf("graph", h.graph))

	router.Get("/heap", instrf("heap", dumpHeap))

	router.Get(o.MetricsPath, prometheus.Handler().ServeHTTP)

	h.apiLegacy.Register(router.WithPrefix("/api"))
	h.apiV1.Register(router.WithPrefix("/api/v1"))

	router.Get("/consoles/*filepath", instrf("consoles", h.consoles))

	if o.UseLocalAssets {
		router.Get("/static/*filepath", instrf("static", route.FileServe("web/blob/static")))
	} else {
		router.Get("/static/*filepath", instrh("static", blob.Handler{}))
	}

	if o.UserAssetsPath != "" {
		router.Get("/user/*filepath", instrf("user", route.FileServe(o.UserAssetsPath)))
	}

	if o.EnableQuit {
		router.Post("/-/quit", h.quit)
	}

	return h
}

// Quit returns the receive-only quit channel.
func (h *Handler) Quit() <-chan struct{} {
	return h.quitCh
}

// Run serves the HTTP endpoints.
func (h *Handler) Run() {
	log.Infof("Listening on %s", h.options.ListenAddress)

	// If we cannot bind to a port, retry after 30 seconds.
	for {
		err := http.ListenAndServe(h.options.ListenAddress, h.router)
		if err != nil {
			log.Errorf("Could not listen on %s: %s", h.options.ListenAddress, err)
		}
		time.Sleep(30 * time.Second)
	}
}

func (h *Handler) alerts(w http.ResponseWriter, r *http.Request) {
	h.muAlerts.Lock()
	defer h.muAlerts.Unlock()

	alerts := h.ruleManager.AlertingRules()
	alertsSorter := byAlertStateSorter{alerts: alerts}
	sort.Sort(alertsSorter)

	alertStatus := AlertStatus{
		AlertingRules: alertsSorter.alerts,
		AlertStateToRowClass: map[rules.AlertState]string{
			rules.StateInactive: "success",
			rules.StatePending:  "warning",
			rules.StateFiring:   "danger",
		},
	}
	h.executeTemplate(w, "alerts", alertStatus)
}

func (h *Handler) consoles(w http.ResponseWriter, r *http.Request) {
	ctx := route.Context(r)
	name := route.Param(ctx, "filepath")

	file, err := http.Dir(h.options.ConsoleTemplatesPath).Open(name)
	if err != nil {
		http.Error(w, err.Error(), http.StatusNotFound)
		return
	}
	text, err := ioutil.ReadAll(file)
	if err != nil {
		http.Error(w, err.Error(), http.StatusInternalServerError)
		return
	}

	// Provide URL parameters as a map for easy use. Advanced users may have need for
	// parameters beyond the first, so provide RawParams.
	rawParams, err := url.ParseQuery(r.URL.RawQuery)
	if err != nil {
		http.Error(w, err.Error(), http.StatusBadRequest)
		return
	}
	params := map[string]string{}
	for k, v := range rawParams {
		params[k] = v[0]
	}
	data := struct {
		RawParams url.Values
		Params    map[string]string
		Path      string
	}{
		RawParams: rawParams,
		Params:    params,
		Path:      name,
	}

	tmpl := template.NewTemplateExpander(string(text), "__console_"+name, data, clientmodel.Now(), h.queryEngine, h.options.PathPrefix)
	filenames, err := filepath.Glob(h.options.ConsoleLibrariesPath + "/*.lib")
	if err != nil {
		http.Error(w, err.Error(), http.StatusInternalServerError)
		return
	}
	result, err := tmpl.ExpandHTML(filenames)
	if err != nil {
		http.Error(w, err.Error(), http.StatusInternalServerError)
		return
	}
	io.WriteString(w, result)
}

func (h *Handler) graph(w http.ResponseWriter, r *http.Request) {
	h.executeTemplate(w, "graph", nil)
}

func (h *Handler) statush(w http.ResponseWriter, r *http.Request) {
	h.status.mu.RLock()
	defer h.status.mu.RUnlock()

	h.executeTemplate(w, "status", h.status)
}

func (h *Handler) quit(w http.ResponseWriter, r *http.Request) {
	fmt.Fprintf(w, "Requesting termination... Goodbye!")
	close(h.quitCh)
}

func (h *Handler) getTemplateFile(name string) (string, error) {
	if h.options.UseLocalAssets {
		file, err := ioutil.ReadFile(fmt.Sprintf("web/blob/templates/%s.html", name))
		if err != nil {
			log.Errorf("Could not read %s template: %s", name, err)
			return "", err
		}
		return string(file), nil
	}
	file, err := blob.GetFile(blob.TemplateFiles, name+".html")
	if err != nil {
		log.Errorf("Could not read %s template: %s", name, err)
		return "", err
	}
	return string(file), nil
}

func (h *Handler) getConsoles() string {
	if _, err := os.Stat(h.options.ConsoleTemplatesPath + "/index.html"); !os.IsNotExist(err) {
		return h.options.PathPrefix + "/consoles/index.html"
	}
	if h.options.UserAssetsPath != "" {
		if _, err := os.Stat(h.options.UserAssetsPath + "/index.html"); !os.IsNotExist(err) {
			return h.options.PathPrefix + "/user/index.html"
		}
	}
	return ""
}

func (h *Handler) getTemplate(name string) (*template_std.Template, error) {
	t := template_std.New("_base")
	var err error

	t.Funcs(template_std.FuncMap{
		"since":       time.Since,
		"getConsoles": h.getConsoles,
		"pathPrefix":  func() string { return h.options.PathPrefix },
		"stripLabels": func(lset clientmodel.LabelSet, labels ...clientmodel.LabelName) clientmodel.LabelSet {
			for _, ln := range labels {
				delete(lset, ln)
			}
			return lset
		},
		"globalURL": func(url string) string {
			for _, localhostRepresentation := range localhostRepresentations {
				url = strings.Replace(url, "//"+localhostRepresentation, "//"+h.options.Hostname, 1)
			}
			return url
		},
		"healthToClass": func(th retrieval.TargetHealth) string {
			switch th {
			case retrieval.HealthUnknown:
				return "warning"
			case retrieval.HealthGood:
				return "success"
			default:
				return "danger"
			}
		},
	})

	file, err := h.getTemplateFile("_base")
	if err != nil {
		log.Errorln("Could not read base template:", err)
		return nil, err
	}
	t, err = t.Parse(file)
	if err != nil {
		log.Errorln("Could not parse base template:", err)
	}

	file, err = h.getTemplateFile(name)
	if err != nil {
		log.Error("Could not read template %s: %s", name, err)
		return nil, err
	}
	t, err = t.Parse(file)
	if err != nil {
		log.Errorf("Could not parse template %s: %s", name, err)
	}
	return t, err
}

func (h *Handler) executeTemplate(w http.ResponseWriter, name string, data interface{}) {
	tpl, err := h.getTemplate(name)
	if err != nil {
		log.Error("Error preparing layout template: ", err)
		return
	}
	err = tpl.Execute(w, data)
	if err != nil {
		log.Error("Error executing template: ", err)
	}
}

func dumpHeap(w http.ResponseWriter, r *http.Request) {
	target := fmt.Sprintf("/tmp/%d.heap", time.Now().Unix())
	f, err := os.Create(target)
	if err != nil {
		log.Error("Could not dump heap: ", err)
	}
	fmt.Fprintf(w, "Writing to %s...", target)
	defer f.Close()
	pprof_runtime.WriteHeapProfile(f)
	fmt.Fprintf(w, "Done")
}

// AlertStatus bundles alerting rules and the mapping of alert states to row classes.
type AlertStatus struct {
	AlertingRules        []*rules.AlertingRule
	AlertStateToRowClass map[rules.AlertState]string
}

type byAlertStateSorter struct {
	alerts []*rules.AlertingRule
}

func (s byAlertStateSorter) Len() int {
	return len(s.alerts)
}

func (s byAlertStateSorter) Less(i, j int) bool {
	return s.alerts[i].State() > s.alerts[j].State()
}

func (s byAlertStateSorter) Swap(i, j int) {
	s.alerts[i], s.alerts[j] = s.alerts[j], s.alerts[i]
}

package main

import (
	"context"
	"flag"
	"fmt"
	"log"
	"net/http"
	"net/url"
	"os"
	"time"

	"github.com/gogo/protobuf/proto"
	"github.com/golang/snappy"
	"github.com/prometheus/client_golang/prometheus"
	"github.com/prometheus/client_golang/prometheus/promhttp"
	dto "github.com/prometheus/client_model/go"
	config_util "github.com/prometheus/common/config"
	"github.com/prometheus/common/expfmt"
	"github.com/prometheus/common/model"
	"github.com/prometheus/prometheus/prompb"
	"github.com/prometheus/prometheus/storage/remote"
)

var (
	version = prometheus.NewGauge(prometheus.GaugeOpts{
		Name: "version",
		Help: "Version information about this binary",
		ConstLabels: map[string]string{
			"version": "v0.1.0",
		},
	})

	alert = prometheus.NewGauge(prometheus.GaugeOpts{
		Name: "alert",
		Help: "for alert purpose",
		ConstLabels: map[string]string{
			"reason": "test",
		},
	})

	httpRequestsTotal = prometheus.NewCounterVec(prometheus.CounterOpts{
		Name: "http_requests_total",
		Help: "Count of all HTTP requests",
	}, []string{"code", "method"})

	testSummary = prometheus.NewSummary(prometheus.SummaryOpts{
		Name: "hello_world",
		ConstLabels: map[string]string{
			"reason": "test",
		},
	})
)

func main() {
	bind := ""
	flagset := flag.NewFlagSet(os.Args[0], flag.ExitOnError)
	flagset.StringVar(&bind, "bind", ":8080", "The socket to bind to.")
	flagset.Parse(os.Args[1:])

	r := prometheus.NewRegistry()
	r.MustRegister(httpRequestsTotal)
	r.MustRegister(version)
	r.MustRegister(alert)
	r.MustRegister(testSummary)

	handler := http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.WriteHeader(http.StatusOK)
		w.Write([]byte("Hello from example application."))
	})
	notfound := http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.WriteHeader(http.StatusNotFound)
	})

	setAlert := http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.WriteHeader(http.StatusOK)
		w.Write([]byte("Alert set."))
		alert.Set(1)
	})

	unSetAlert := http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.WriteHeader(http.StatusOK)
		w.Write([]byte("Alert unset."))
		alert.Set(0)
	})

	http.Handle("/", promhttp.InstrumentHandlerCounter(httpRequestsTotal, handler))
	http.Handle("/err", promhttp.InstrumentHandlerCounter(httpRequestsTotal, notfound))
	http.Handle("/alert/set", setAlert)
	http.Handle("/alert/unset", unSetAlert)

	http.Handle("/metrics", promhttp.HandlerFor(r, promhttp.HandlerOpts{}))

	// remote write part
	u, err := url.Parse("http://192.168.99.100:30080/api/prom/push")
	if err != nil {
		log.Fatal(err)
	}

	dur, err := model.ParseDuration("50s")
	if err != nil {
		log.Fatal(err)
	}

	conf := remote.ClientConfig{
		URL: &config_util.URL{
			u,
		},
		Timeout: dur,
		HTTPClientConfig: config_util.HTTPClientConfig{
			TLSConfig: config_util.TLSConfig{
				InsecureSkipVerify: true,
			},
		},
	}

	cl, err := remote.NewClient(0, &conf)
	if err != nil {
		log.Fatal(err)
	}
	stopCh := make(chan struct{})
	ctx := context.Background()

	go remoteWrite(cl, ctx, r, stopCh)

	fmt.Println("running server..........")
	if err := http.ListenAndServe(bind, nil); err != nil {
		close(stopCh)
		log.Fatal(err)
	} else {
		close(stopCh)
	}
}

// It will write data in every 5s
func remoteWrite(cl *remote.Client, ctx context.Context, r prometheus.Gatherer, stopCh chan struct{}) {
	for {
		select {
		case <-time.After(5 * time.Second):
			mfs, err := r.Gather()
			if err != nil {
				log.Println(err)
				continue
			}

			samples, err := metricFamilyToTimeseries(mfs)
			if err != nil {
				log.Println(err)
				continue
			}

			req, err := buildWriteRequest(samples)
			if err != nil {
				log.Println(err)
				continue
			}

			err = cl.Store(ctx, req)
			if err != nil {
				log.Println(err)
				continue
			}

			fmt.Println("pushed data....")
		case <-stopCh:
			return
		}
	}
}

func metricFamilyToTimeseries(mfs []*dto.MetricFamily) ([]prompb.TimeSeries, error) {
	ts := []prompb.TimeSeries{}
	for _, mf := range mfs {
		vec, err := expfmt.ExtractSamples(&expfmt.DecodeOptions{
			model.Now(),
		}, mf)
		if err != nil {
			return nil, err
		}

		for _, s := range vec {
			if s != nil {
				ts = append(ts, prompb.TimeSeries{
					Labels: metricToLabels(s.Metric),
					Samples: []prompb.Sample{
						{
							Value:     float64(s.Value),
							Timestamp: int64(s.Timestamp),
						},
					},
				})
			}
		}
	}
	return ts, nil
}

func metricToLabels(m model.Metric) []prompb.Label {
	lables := []prompb.Label{}
	for k, v := range m {
		lables = append(lables, prompb.Label{
			Name:  string(k),
			Value: string(v),
		})
	}
	return lables
}

// https://github.com/prometheus/prometheus/blob/84df210c410a0684ec1a05479bfa54458562695e/storage/remote/queue_manager.go#L759
func buildWriteRequest(samples []prompb.TimeSeries) ([]byte, error) {
	req := &prompb.WriteRequest{
		Timeseries: samples,
	}

	data, err := proto.Marshal(req)
	if err != nil {
		return nil, err
	}

	compressed := snappy.Encode(nil, data)
	return compressed, nil
}

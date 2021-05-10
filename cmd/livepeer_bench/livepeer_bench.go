package main

import (
	"bufio"
	"encoding/json"
	"flag"
	"fmt"
	"io/ioutil"
	"os"
	"path"
	"strconv"
	"strings"
	"sync"
	"time"

	//"runtime/pprof"

	"github.com/golang/glog"
	"github.com/livepeer/go-livepeer/common"
	"github.com/livepeer/lpms/ffmpeg"
	"github.com/livepeer/m3u8"
	"github.com/olekukonko/tablewriter"
)

func main() {
	// Override the default flag set since there are dependencies that
	// incorrectly add their own flags (specifically, due to the 'testing'
	// package being linked)
	flag.Set("logtostderr", "true")
	flag.CommandLine = flag.NewFlagSet(os.Args[0], flag.ExitOnError)

	in := flag.String("in", "", "Input m3u8 manifest file")
	live := flag.Bool("live", true, "Simulate live stream")
	concurrentSessions := flag.Int("concurrentSessions", 1, "# of concurrent transcode sessions")
	segs := flag.Int("segs", 0, "Maximum # of segments to transcode (default all)")
	transcodingOptions := flag.String("transcodingOptions", "P240p30fps16x9,P360p30fps16x9,P720p30fps16x9", "Transcoding options for broadcast job, or path to json config")
	nvidia := flag.String("nvidia", "", "Comma-separated list of Nvidia GPU device IDs to use for transcoding")
	outPrefix := flag.String("outPrefix", "", "Output segments' prefix (no segments are generated by default)")

	flag.Parse()

	if *in == "" {
		glog.Errorf("Please provide the input manifest as `%s -in <input.m3u8>`", os.Args[0])
		flag.Usage()
		os.Exit(1)
	}

	profiles := parseVideoProfiles(*transcodingOptions)
	f, err := os.Open(*in)
	if err != nil {
		glog.Fatal("Couldn't open input manifest: ", err)
	}
	p, _, err := m3u8.DecodeFrom(bufio.NewReader(f), true)
	if err != nil {
		glog.Fatal("Couldn't decode input manifest: ", err)
	}
	pl, ok := p.(*m3u8.MediaPlaylist)
	if !ok {
		glog.Fatalf("Expecting media playlist in the input %s", *in)
	}

	accel := ffmpeg.Software
	devices := []string{}
	if *nvidia != "" {
		accel = ffmpeg.Nvidia
		devices = strings.Split(*nvidia, ",")
	}

	ffmpeg.InitFFmpeg()
	var wg sync.WaitGroup
	dir := path.Dir(*in)

	table := tablewriter.NewWriter(os.Stderr)
	data := [][]string{
		{"Source File", *in},
		{"Transcoding Options", *transcodingOptions},
		{"Concurrent Sessions", fmt.Sprintf("%v", *concurrentSessions)},
		{"Live Mode", fmt.Sprintf("%v", *live)},
	}

	table.SetAlignment(tablewriter.ALIGN_LEFT)
	table.SetCenterSeparator("*")
	table.SetColumnSeparator("|")
	table.AppendBulk(data)
	table.Render()

	fmt.Println("timestamp,session,segment,seg_dur,transcode_time")
	segCount := 0
	realTimeSegCount := 0
	srcDur := 0.0
	var mu sync.Mutex
	transcodeDur := 0.0
	for i := 0; i < *concurrentSessions; i++ {
		wg.Add(1)
		go func(k int, wg *sync.WaitGroup) {
			tc := ffmpeg.NewTranscoder()
			for j, v := range pl.Segments {
				iterStart := time.Now()
				if *segs > 0 && j >= *segs {
					break
				}
				if v == nil {
					continue
				}
				u := path.Join(dir, v.URI)
				in := &ffmpeg.TranscodeOptionsIn{
					Fname: u,
					Accel: accel,
				}
				if ffmpeg.Software != accel {
					in.Device = devices[k%len(devices)]
				}
				profs2opts := func(profs []ffmpeg.VideoProfile) []ffmpeg.TranscodeOptions {
					opts := []ffmpeg.TranscodeOptions{}
					for n, p := range profs {
						oname := ""
						muxer := ""
						if *outPrefix != "" {
							oname = fmt.Sprintf("%s_%s_%d_%d_%d.ts", *outPrefix, p.Name, n, k, j)
							muxer = "mpegts"
						} else {
							oname = "-"
							muxer = "null"
						}
						o := ffmpeg.TranscodeOptions{
							Oname:        oname,
							Profile:      p,
							Accel:        accel,
							AudioEncoder: ffmpeg.ComponentOptions{Name: "drop"},
							Muxer:        ffmpeg.ComponentOptions{Name: muxer},
						}
						opts = append(opts, o)
					}
					return opts
				}
				out := profs2opts(profiles)
				t := time.Now()
				_, err := tc.Transcode(in, out)
				end := time.Now()
				if err != nil {
					glog.Fatalf("Transcoding failed for session %d segment %d: %v", k, j, err)
				}
				fmt.Printf("%s,%d,%d,%0.4v,%0.4v\n", end.Format("2006-01-02 15:04:05.9999"), k, j, v.Duration, end.Sub(t).Seconds())
				segTxDur := end.Sub(t).Seconds()
				mu.Lock()
				transcodeDur += segTxDur
				srcDur += v.Duration
				segCount++
				if segTxDur <= v.Duration {
					realTimeSegCount += 1
				}
				mu.Unlock()
				iterEnd := time.Now()
				segDur := time.Duration(v.Duration * float64(time.Second))
				if *live {
					time.Sleep(segDur - iterEnd.Sub(iterStart))
				}
			}
			tc.StopTranscoder()
			wg.Done()
		}(i, &wg)
		time.Sleep(300 * time.Millisecond)
	}
	wg.Wait()
	if segCount == 0 || srcDur == 0.0 {
		glog.Fatal("Input manifest has no segments or total duration is 0s")
	}
	statsTable := tablewriter.NewWriter(os.Stderr)
	stats := [][]string{
		{"Concurrent Sessions", fmt.Sprintf("%v", *concurrentSessions)},
		{"Total Segs Transcoded", fmt.Sprintf("%v", segCount)},
		{"Real-Time Segs Transcoded", fmt.Sprintf("%v", realTimeSegCount)},
		{"* Real-Time Segs Ratio *", fmt.Sprintf("%0.4v", float64(realTimeSegCount)/float64(segCount))},
		{"Total Source Duration", fmt.Sprintf("%vs", srcDur)},
		{"Total Transcoding Duration", fmt.Sprintf("%vs", transcodeDur)},
		{"* Real-Time Duration Ratio *", fmt.Sprintf("%0.4v", transcodeDur/srcDur)},
	}

	statsTable.SetAlignment(tablewriter.ALIGN_LEFT)
	statsTable.SetCenterSeparator("*")
	statsTable.SetColumnSeparator("|")
	statsTable.AppendBulk(stats)
	statsTable.Render()
}

func parseVideoProfiles(inp string) []ffmpeg.VideoProfile {
	type profilesJson struct {
		Profiles []struct {
			Name    string `json:"name"`
			Width   int    `json:"width"`
			Height  int    `json:"height"`
			Bitrate int    `json:"bitrate"`
			FPS     uint   `json:"fps"`
			FPSDen  uint   `json:"fpsDen"`
			Profile string `json:"profile"`
			GOP     string `json:"gop"`
		} `json:"profiles"`
	}
	profs := []ffmpeg.VideoProfile{}
	if inp != "" {
		// try opening up json file with profiles
		content, err := ioutil.ReadFile(inp)
		if err == nil && len(content) > 0 {
			// parse json profiles
			resp := &profilesJson{}
			err = json.Unmarshal(content, &resp.Profiles)
			if err != nil {
				glog.Fatal("Unable to unmarshal the passed transcoding option: ", err)
			}
			for _, profile := range resp.Profiles {
				name := profile.Name
				if name == "" {
					name = "custom_" + common.DefaultProfileName(
						profile.Width,
						profile.Height,
						profile.Bitrate)
				}
				var gop time.Duration
				if profile.GOP != "" {
					if profile.GOP == "intra" {
						gop = ffmpeg.GOPIntraOnly
					} else {
						gopFloat, err := strconv.ParseFloat(profile.GOP, 64)
						if err != nil {
							glog.Fatal("Cannot parse the GOP value in the transcoding options: ", err)
						}
						if gopFloat <= 0.0 {
							glog.Fatalf("Invalid gop value %f. Please set it to a positive value", gopFloat)
						}
						gop = time.Duration(gopFloat * float64(time.Second))
					}
				}
				encodingProfile, err := common.EncoderProfileNameToValue(profile.Profile)
				if err != nil {
					glog.Fatal("Unable to parse the H264 encoder profile: ", err)
				}
				prof := ffmpeg.VideoProfile{
					Name:         name,
					Bitrate:      fmt.Sprint(profile.Bitrate),
					Framerate:    profile.FPS,
					FramerateDen: profile.FPSDen,
					Resolution:   fmt.Sprintf("%dx%d", profile.Width, profile.Height),
					Profile:      encodingProfile,
					GOP:          gop,
				}
				profs = append(profs, prof)
			}
		} else {
			// check the built-in profiles
			profs = make([]ffmpeg.VideoProfile, 0)
			presets := strings.Split(inp, ",")
			for _, v := range presets {
				if p, ok := ffmpeg.VideoProfileLookup[strings.TrimSpace(v)]; ok {
					profs = append(profs, p)
				}
			}
		}
		if len(profs) <= 0 {
			glog.Fatalf("No transcoding options provided")
		}
	}
	return profs
}

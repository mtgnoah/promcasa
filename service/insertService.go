package service

import (
	"database/sql"
	"fmt"
	"math/rand"
	"sync"
	"time"

	"github.com/ClickHouse/clickhouse-go"
	"github.com/Jeffail/gabs/v2"
	"github.com/metrico/promcasa/config"
	"github.com/metrico/promcasa/model"
	"github.com/metrico/promcasa/utils/function"
	heputils "github.com/metrico/promcasa/utils/heputils"
	"github.com/metrico/promcasa/utils/logger"
	"github.com/patrickmn/go-cache"
)

type TableSamples struct {
	Samples []*model.TableSample
	Resp    chan error
}

type TableTimeSeriesReq struct {
	TimeSeries []*model.TableTimeSeries
	Resp       chan error
}

type InsertService struct {
	ServiceData
	GoCache         *cache.Cache
	DatabaseNodeMap *[]model.DataDatabasesMap
	TSCh            []chan *TableTimeSeriesReq
	SPCh            []chan *TableSamples
	SamplesChans    [][]chan error
	TimeSeriesChans [][]chan error
}

func (ss *InsertService) InsertTimeSeries() {

	wg := sync.WaitGroup{}
	timerInterval, _ := time.ParseDuration(config.Setting.SYSTEM_SETTINGS.DBTimer)

	for idx, tsCh := range ss.TSCh {
		wg.Add(1)

		go func(idx int, tsCh chan *TableTimeSeriesReq) {

			var txTS *sql.Tx
			var stmtTS *sql.Stmt
			var tsCnt int
			var err error

			sqlTS := fmt.Sprintf("INSERT INTO %s (%s) VALUES (%s)", (*ss.DatabaseNodeMap)[config.Setting.CurrentDataBaseIndex].TableSeries,
				function.FieldName(function.DBFields(model.TableTimeSeries{})), function.FieldValue(function.DBFields(model.TableTimeSeries{})))

			timer := time.NewTimer(timerInterval)
			stop := func() {
				if !timer.Stop() {
					select {
					case <-timer.C:
					default:
					}
				}
			}

			defer stop()
			defer wg.Done()

			for {
				select {
				case ts, ok := <-tsCh:

					if !ok {
						logger.Error("Bad tsc channel index: ", idx)
						break
					}

					if tsCnt == 0 {

						if !(*ss.DatabaseNodeMap)[config.Setting.CurrentDataBaseIndex].Online {
							logger.Error("db is offline tsCnt: ")
							return
						}

						txTS, err = ss.Session[config.Setting.CurrentDataBaseIndex].Begin()
						if err != nil {
							logger.Error("error during begin txTS: ", err)
							break
						}
						stmtTS, err = txTS.Prepare(sqlTS)
						if err != nil {
							logger.Error("prepare ts: ", err)
							break
						}

					}
					for _, s := range ts.TimeSeries {
						stmtTS.Exec(function.GenerateArg(s)...)
						tsCnt++
					}
					ss.TimeSeriesChans[idx] = append(ss.TimeSeriesChans[idx], ts.Resp)

					if tsCnt >= config.Setting.SYSTEM_SETTINGS.DBBulk {
						err := txTS.Commit()
						if err != nil {
							logger.Error("error during commit txTS [1]: ", err)
						}
						for _, c := range ss.TimeSeriesChans[idx] {
							c <- err
							close(c)
						}
						ss.TimeSeriesChans[idx] = ss.TimeSeriesChans[idx][0:0]
						tsCnt = 0
					}

				case <-timer.C:
					timer.Reset(timerInterval)
					switch {
					case tsCnt > 0:
						err := txTS.Commit()
						if err != nil {
							logger.Error("error during commit txTS [2]: ", err)
						}
						for _, c := range ss.TimeSeriesChans[idx] {
							c <- err
							close(c)
						}
						ss.TimeSeriesChans[idx] = ss.TimeSeriesChans[idx][0:0]
						tsCnt = 0

						lenTimeSeries := uint32(len(tsCh))
						if lenTimeSeries >= (config.Setting.SYSTEM_SETTINGS.BufferSizeTimeSeries - 10) {
							logger.Error("Timeseries buffer is overloaded. Index: ", idx, ", Len: ", lenTimeSeries)
						}
					}
				}
			}

		}(idx, tsCh)

	}
	wg.Wait()
}

func (ss *InsertService) InsertTableSamples(sample []*model.TableSample) chan error {
	rand.Seed(time.Now().UnixNano())
	index := rand.Intn(config.Setting.SYSTEM_SETTINGS.ChannelsSample - 0 + 1)
	res := make(chan error)
	ss.SPCh[index] <- &TableSamples{sample, res}
	return res
}

func (ss *InsertService) InsertTimeSeriesRequest(sample []*model.TableTimeSeries) chan error {
	rand.Seed(time.Now().UnixNano())
	index := rand.Intn(config.Setting.SYSTEM_SETTINGS.ChannelsSample - 0 + 1)
	res := make(chan error)
	ss.TSCh[index] <- &TableTimeSeriesReq{sample, res}
	return res
}

func (ss *InsertService) InsertSamples() {

	wg := sync.WaitGroup{}
	timerInterval, _ := time.ParseDuration(config.Setting.SYSTEM_SETTINGS.DBTimer)

	for idx, spCh := range ss.SPCh {
		wg.Add(1)
		go func(idx int, spCh chan *TableSamples) {

			var txSP *sql.Tx
			var stmtSP *sql.Stmt
			var spCnt int
			var err error

			timer := time.NewTimer(timerInterval)
			stop := func() {
				if !timer.Stop() {
					select {
					case <-timer.C:
					default:
					}
				}
			}
			defer stop()

			sqlSP := fmt.Sprintf("INSERT INTO %s (%s) VALUES (%s)", (*ss.DatabaseNodeMap)[config.Setting.CurrentDataBaseIndex].TableSamples,
				function.FieldName(function.DBFields(model.TableSample{})), function.FieldValue(function.DBFields(model.TableSample{})))

			defer wg.Done()
			for {
				select {

				case sample, ok := <-spCh:

					if !ok {
						logger.Error("Bad sample channel index:", idx)
					}

					if spCnt == 0 {

						if !(*ss.DatabaseNodeMap)[config.Setting.CurrentDataBaseIndex].Online {
							logger.Error("db is offline spCnt: ")
						}

						txSP, err = ss.Session[config.Setting.CurrentDataBaseIndex].Begin()
						if err != nil {
							logger.Error("session txSP begin has error: ", err)
						}
						stmtSP, err = txSP.Prepare(sqlSP)
						if err != nil {
							logger.Error(err)
						}
					}
					for _, s := range sample.Samples {
						stmtSP.Exec(function.GenerateArg(s)...)
						spCnt++
					}
					ss.SamplesChans[idx] = append(ss.SamplesChans[idx], sample.Resp)
					if spCnt >= config.Setting.SYSTEM_SETTINGS.DBBulk {
						err := txSP.Commit()
						if err != nil {
							logger.Error("commmit txSP has error [1]: ", err)
						}
						for _, c := range ss.SamplesChans[idx] {
							c <- err
							close(c)
						}
						ss.SamplesChans[idx] = ss.SamplesChans[idx][0:0]
						spCnt = 0
					}

				case <-timer.C:
					timer.Reset(timerInterval)
					switch {
					case spCnt > 0:
						err = txSP.Commit()
						if err != nil {
							logger.Error("commmit txSP has error [2]: ", err)
						}
						for _, c := range ss.SamplesChans[idx] {
							c <- err
							close(c)
						}
						ss.SamplesChans[idx] = ss.SamplesChans[idx][0:0]
						spCnt = 0

						lenSamples := uint32(len(spCh))
						if lenSamples >= (config.Setting.SYSTEM_SETTINGS.BufferSizeSample - 10) {
							logger.Error("Samples buffer is overloaded. Index: ", idx, ", Len: ", lenSamples)
						}
					}
				}
			}
		}(idx, spCh)
	}
	wg.Wait()
}

// this method checks the structure of incoming stream and create batch
/*
func (ss *InsertService) PushStream(req model.PushRequest) error {

	for _, stream := range req.Streams {
		labelsArr := strings.Split(stream.Labels[1:len(stream.Labels)-1], ",")

		lbs := make([]model.Label, len(labelsArr))
		labelKey := make([]string, len(labelsArr))

		labelValue := make(map[string][]string)
		for k, l := range labelsArr {
			keyValue := strings.SplitN(l, "=", 2)
			value, _ := strconv.Unquote(keyValue[1])
			labelKey[k] = keyValue[0]
			labelValue[keyValue[0]] = append(labelValue[keyValue[0]], value)
			lbs[k] = model.Label{
				Key:   keyValue[0],
				Value: value,
			}
		}

		// lets insert only the unique values for key
		for k, v := range labelValue {
			if keys, exist := ss.GoCache.Get(k); exist {
				ss.GoCache.Replace(k, heputils.AppendTwoSlices(keys.([]string), heputils.UniqueSlice(v)), 0)
			} else {
				ss.GoCache.Add(k, heputils.UniqueSlice(v), 0)
			}
		}

		sort.Slice(lbs[:], func(i, j int) bool {
			return lbs[i].Key < lbs[j].Key
		})

		fingerPrint := heputils.FingerprintLabels(lbs)

		// if fingerprint was not found, lets insert into time_series
		if _, found := ss.GoCache.Get(fmt.Sprint(fingerPrint)); !found {
			if keys, exist := ss.GoCache.Get("__LABEL__"); exist {
				labelKeys := keys.([]string)
				uniqueKeys := heputils.AppendTwoSlices(labelKeys, labelKey)
				ss.GoCache.Replace("__LABEL__", uniqueKeys, 0)

			} else {
				ss.GoCache.Add("__LABEL__", labelKey, 0)
			}

			b := bytebufferpool.Get()

			ss.GoCache.Set(fmt.Sprint(fingerPrint), true, cache.DefaultExpiration)

			rand.Seed(time.Now().UnixNano())
			index := rand.Intn(config.Setting.SYSTEM_SETTINGS.ChannelsTimeSeries - 0 + 1)

			ss.TSCh[index] <- &model.TableTimeSeries{
				Date:        time.Now(),
				FingerPrint: fingerPrint,
				Labels:      heputils.MakeJson(lbs, b),
				Name:        "",
			}

		}

		for _, _ = range stream.Entries {

			rand.Seed(time.Now().UnixNano())
			//index := rand.Intn(config.Setting.SYSTEM_SETTINGS.ChannelsSample - 0 + 1)

			ss.SPCh[index] <- &model.TableSample{
				FingerPrint: fingerPrint,
				TimestampMS: time.Now().UnixNano() / 1000000,
				Value:       0,
				String:      entries.Line,
			}

		}
	}
	return nil
}
*/

// this method create new user in the database
// it doesn't check internally whether all the validation are applied or not
func (ss *InsertService) ReloadFingerprints() error {

	if !(*ss.DatabaseNodeMap)[config.Setting.CurrentDataBaseIndex].Online {
		logger.Error("the node is offline:")
		return fmt.Errorf("the node is offline")
	}

	rows, err := ss.Session[config.Setting.CurrentDataBaseIndex].Queryx("SELECT DISTINCT fingerprint, labels FROM ?", (*ss.DatabaseNodeMap)[config.Setting.CurrentDataBaseIndex].TableSeries) // (*sql.Rows, error)
	if err != nil {
		logger.Error("couldn't select alias data: ", err.Error())
	}

	defer rows.Close()
	var labels []string
	for rows.Next() {
		var label string
		var finerprint uint64
		rows.Scan(&finerprint, &label)
		labels = append(labels, label)

	}

	for _, label := range labels {
		lb, _ := gabs.ParseJSON([]byte(label))
		var labelKey []string
		labelValue := make(map[string][]string)
		for lk, lv := range lb.ChildrenMap() {
			labelKey = append(labelKey, lk)
			labelValue[lk] = append(labelValue[lk], lv.Data().(string))
		}

		// lets have only unique values for a label keys
		for k, v := range labelValue {
			if keys, exist := ss.GoCache.Get(k); exist {
				ss.GoCache.Replace(k, heputils.AppendTwoSlices(keys.([]string), heputils.UniqueSlice(v)), 0)
			} else {
				ss.GoCache.Add(k, heputils.UniqueSlice(v), 0)
			}
		}

		// lets have only unique label keys
		if keys, exist := ss.GoCache.Get("__LABEL__"); exist {
			labelKeys := keys.([]string)
			uniqueKeys := heputils.AppendTwoSlices(labelKeys, labelKey)
			ss.GoCache.Replace("__LABEL__", uniqueKeys, 0)

		} else {
			ss.GoCache.Add("__LABEL__", labelKey, 0)
		}

	}
	return nil
}

// internal sync
func (ss *InsertService) RunWatcherConfigDatabaseStats() error {

	//var searchData
	for idx, db := range ss.Session {
		logger.Debug("RunWatcherConfigDatabaseStats: CHECK DataDB: ", (*ss.DatabaseNodeMap)[idx].Name)

		if err := db.Ping(); err != nil {
			(*ss.DatabaseNodeMap)[idx].Online = false
			logger.Debug("node is offline: ", (*ss.DatabaseNodeMap)[idx].Name)
			if exception, ok := err.(*clickhouse.Exception); ok {
				logger.Error(fmt.Sprintf("[%d] %s \n%s\n", exception.Code, exception.Message, exception.StackTrace))
			} else {
				logger.Debug("ping db data ", err)
			}
		} else {
			logger.Debug("node is online: ", (*ss.DatabaseNodeMap)[idx].Name)
			(*ss.DatabaseNodeMap)[idx].Online = true

		}
	}

	return nil
}

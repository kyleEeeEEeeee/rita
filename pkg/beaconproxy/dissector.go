package beaconproxy

import (
	"sync"

	"github.com/activecm/rita/config"
	"github.com/activecm/rita/database"
	"github.com/activecm/rita/pkg/uconnproxy"
	"github.com/globalsign/mgo/bson"
)

type (
	dissector struct {
		connLimit         int64                   // limit for strobe classification
		db                *database.DB            // provides access to MongoDB
		conf              *config.Config          // contains details needed to access MongoDB
		dissectedCallback func(*uconnproxy.Input) // called on each analyzed result
		closedCallback    func()                  // called when .close() is called and no more calls to analyzedCallback will be made
		dissectChannel    chan *uconnproxy.Input  // holds unanalyzed data
		dissectWg         sync.WaitGroup          // wait for analysis to finish
	}
)

//newdissector creates a new collector for gathering data
func newDissector(connLimit int64, db *database.DB, conf *config.Config, dissectedCallback func(*uconnproxy.Input), closedCallback func()) *dissector {
	return &dissector{
		connLimit:         connLimit,
		db:                db,
		conf:              conf,
		dissectedCallback: dissectedCallback,
		closedCallback:    closedCallback,
		dissectChannel:    make(chan *uconnproxy.Input),
	}
}

//collect sends a chunk of data to be analyzed
func (d *dissector) collect(entry *uconnproxy.Input) {
	d.dissectChannel <- entry
}

//close waits for the collector to finish
func (d *dissector) close() {
	close(d.dissectChannel)
	d.dissectWg.Wait()
	d.closedCallback()
}

//start kicks off a new analysis thread
func (d *dissector) start() {
	d.dissectWg.Add(1)
	go func() {
		ssn := d.db.Session.Copy()
		defer ssn.Close()

		for datum := range d.dissectChannel {

			matchNoStrobeKey := datum.Hosts.BSONKey()

			// we are able to filter out already flagged strobes here
			// because we use the uconnproxy table to access them. The uconnproxy table has
			// already had its counts and stats updated.
			matchNoStrobeKey["strobe"] = bson.M{"$ne": true}

			// This will work for both updating and inserting completely new proxy beacons
			// for every new uconnproxy record we have, we will check the uconnproxy table. This
			// will always return a result because even with a brand new database, we already
			// created the uconnproxy table. It will only continue and analyze if the connection
			// meets the required specs, again working for both an update and a new src-fqdn pair.
			// We would have to perform this check regardless if we want the rolling update
			// option to remain, and this gets us the vetting for both situations, and Only
			// works on the current entries - not a re-aggregation on the whole collection,
			// and individual lookups like this are really fast. This also ensures a unique
			// set of timestamps for analysis.
			uconnProxyFindQuery := []bson.M{
				{"$match": matchNoStrobeKey},
				{"$limit": 1},
				{"$project": bson.M{
					"ts":    "$dat.ts",
					"count": "$dat.count",
				}},
				{"$unwind": "$count"},
				{"$group": bson.M{
					"_id":   "$_id",
					"ts":    bson.M{"$first": "$ts"},
					"count": bson.M{"$sum": "$count"},
				}},
				{"$match": bson.M{"count": bson.M{"$gt": d.conf.S.BeaconProxy.DefaultConnectionThresh}}},
				{"$unwind": "$ts"},
				{"$unwind": "$ts"},
				{"$group": bson.M{
					"_id":     "$_id",
					"ts":      bson.M{"$addToSet": "$ts"},
					"ts_full": bson.M{"$push": "$ts"},
					"count":   bson.M{"$first": "$count"},
				}},
				{"$project": bson.M{
					"_id":     "$_id",
					"ts":      1,
					"ts_full": 1,
					"count":   1,
				}},
			}

			var res struct {
				Count  int64   `bson:"count"`
				Ts     []int64 `bson:"ts"`
				TsFull []int64 `bson:"ts_full"`
			}

			_ = ssn.DB(d.db.GetSelectedDB()).C(d.conf.T.Structure.UniqueConnProxyTable).Pipe(uconnProxyFindQuery).AllowDiskUse().One(&res)

			// Check for errors and parse results
			// this is here because it will still return an empty document even if there are no results
			if res.Count > 0 {
				analysisInput := &uconnproxy.Input{
					Hosts:           datum.Hosts,
					Proxy:           datum.Proxy,
					ConnectionCount: res.Count,
				}

				// check if uconnproxy has become a strobe
				if analysisInput.ConnectionCount > d.connLimit {

					// set to sorter channel
					d.dissectedCallback(analysisInput)

				} else { // otherwise, parse timestamps

					analysisInput.TsList = res.Ts
					analysisInput.TsListFull = res.TsFull

					// send to sorter channel if we have over UNIQUE 3 timestamps (analysis needs this verification)
					if len(analysisInput.TsList) > 3 {
						d.dissectedCallback(analysisInput)
					}

				}
			}
		}
		d.dissectWg.Done()
	}()
}

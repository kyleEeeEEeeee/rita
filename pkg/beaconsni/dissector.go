package beaconsni

import (
	"sync"

	"github.com/activecm/rita/config"
	"github.com/activecm/rita/database"
	"github.com/activecm/rita/pkg/data"
	"github.com/globalsign/mgo/bson"
)

type (
	//dissector gathers all of the connection details between a host and an SNI
	dissector struct {
		connLimit         int64                       // limit for strobe classification
		db                *database.DB                // provides access to MongoDB
		conf              *config.Config              // contains details needed to access MongoDB
		dissectedCallback func(dissectorResults)      // gathered SNI connection details are sent to this callback
		closedCallback    func()                      // called when .close() is called and no more calls to dissectedCallback will be made
		dissectChannel    chan data.UniqueSrcFQDNPair // holds data to be processed
		dissectWg         sync.WaitGroup              // wait for dissector to finish
	}
)

//newDissector creates a new dissector for gathering data
func newDissector(connLimit int64, db *database.DB, conf *config.Config, dissectedCallback func(dissectorResults), closedCallback func()) *dissector {
	return &dissector{
		connLimit:         connLimit,
		db:                db,
		conf:              conf,
		dissectedCallback: dissectedCallback,
		closedCallback:    closedCallback,
		dissectChannel:    make(chan data.UniqueSrcFQDNPair),
	}
}

//collect gathers a pair of hosts to obtain SNI connection data for
func (d *dissector) collect(datum data.UniqueSrcFQDNPair) {
	d.dissectChannel <- datum
}

//close waits for the dissector to finish
func (d *dissector) close() {
	close(d.dissectChannel)
	d.dissectWg.Wait()
	d.closedCallback()
}

//start kicks off a new dissector thread
func (d *dissector) start() {
	d.dissectWg.Add(1)
	go func() {
		ssn := d.db.Session.Copy()
		defer ssn.Close()

		for datum := range d.dissectChannel {

			matchNoStrobeKey := datum.BSONKey()

			// we are able to filter out already flagged strobes here
			// because we use the sniconns table to access them. The sniconns table has
			// already had its counts and stats updated.
			matchNoStrobeKey["dat.tls.strobe"] = bson.M{"$ne": true}
			matchNoStrobeKey["dat.http.strobe"] = bson.M{"$ne": true}
			matchNoStrobeKey["dat.merged.strobe"] = bson.M{"$ne": true}

			sniconnFindQuery := []bson.M{
				{"$match": matchNoStrobeKey},
				{"$limit": 1},
				{"$project": bson.M{
					"ts":             bson.M{"$concatArrays": []string{"$dat.http.ts", "$dat.tls.ts"}},
					"bytes":          bson.M{"$concatArrays": []string{"$dat.http.bytes", "$dat.tls.bytes"}},
					"count":          bson.M{"$concatArrays": []string{"$dat.http.count", "$dat.tls.count"}},
					"tbytes":         bson.M{"$concatArrays": []string{"$dat.http.tbytes", "$dat.tls.tbytes"}},
					"responding_ips": bson.M{"$concatArrays": []string{"$dat.http.dst_ips", "$dat.tls.dst_ips"}},
				}},
				{"$unwind": "$count"},
				{"$group": bson.M{
					"_id":            "$_id",
					"ts":             bson.M{"$first": "$ts"},
					"bytes":          bson.M{"$first": "$bytes"},
					"count":          bson.M{"$sum": "$count"},
					"tbytes":         bson.M{"$first": "$tbytes"},
					"responding_ips": bson.M{"$first": "$responding_ips"},
				}},
				{"$match": bson.M{"count": bson.M{"$gt": d.conf.S.BeaconSNI.DefaultConnectionThresh}}},
				{"$unwind": "$tbytes"},
				{"$group": bson.M{
					"_id":            "$_id",
					"ts":             bson.M{"$first": "$ts"},
					"bytes":          bson.M{"$first": "$bytes"},
					"count":          bson.M{"$first": "$count"},
					"tbytes":         bson.M{"$sum": "$tbytes"},
					"responding_ips": bson.M{"$first": "$responding_ips"},
				}},
				{"$unwind": "$ts"},
				{"$unwind": "$ts"},
				{"$group": bson.M{
					"_id":            "$_id",
					"ts":             bson.M{"$addToSet": "$ts"},
					"ts_full":        bson.M{"$push": "$ts"},
					"bytes":          bson.M{"$first": "$bytes"},
					"count":          bson.M{"$first": "$count"},
					"tbytes":         bson.M{"$first": "$tbytes"},
					"responding_ips": bson.M{"$first": "$responding_ips"},
				}},
				{"$unwind": "$bytes"},
				{"$unwind": "$bytes"},
				{"$group": bson.M{
					"_id":            "$_id",
					"ts":             bson.M{"$first": "$ts"},
					"ts_full":        bson.M{"$first": "$ts_full"},
					"bytes":          bson.M{"$push": "$bytes"},
					"count":          bson.M{"$first": "$count"},
					"tbytes":         bson.M{"$first": "$tbytes"},
					"responding_ips": bson.M{"$first": "$responding_ips"},
				}},
				{"$unwind": "$responding_ips"},
				{"$unwind": "$responding_ips"},
				{"$group": bson.M{
					"_id": bson.M{
						"sniconn_id":       "$_id",
						"dst_ip":           "$responding_ips.ip",
						"dst_network_uuid": "$responding_ips.network_uuid",
					},
					"ts":               bson.M{"$first": "$ts"},
					"ts_full":          bson.M{"$first": "$ts_full"},
					"bytes":            bson.M{"$first": "$bytes"},
					"count":            bson.M{"$first": "$count"},
					"tbytes":           bson.M{"$first": "$tbytes"},
					"dst_network_name": bson.M{"$last": "$responding_ips.network_name"},
				}},
				{"$group": bson.M{
					"_id":     "$_id.sniconn_id",
					"ts":      bson.M{"$first": "$ts"},
					"ts_full": bson.M{"$first": "$ts_full"},
					"bytes":   bson.M{"$first": "$bytes"},
					"count":   bson.M{"$first": "$count"},
					"tbytes":  bson.M{"$first": "$tbytes"},
					"responding_ips": bson.M{"$push": bson.M{
						"ip":           "$_id.dst_ip",
						"network_uuid": "$_id.dst_network_uuid",
						"network_name": "$dst_network_name",
					}},
				}},
				{"$project": bson.M{
					"_id":            "$_id",
					"ts":             1,
					"ts_full":        1,
					"bytes":          1,
					"count":          1,
					"tbytes":         1,
					"responding_ips": 1,
				}},
			}

			var res struct {
				Count         int64           `bson:"count"`
				Ts            []int64         `bson:"ts"`
				TsFull        []int64         `bson:"ts_full"`
				Bytes         []int64         `bson:"bytes"`
				TBytes        int64           `bson:"tbytes"`
				RespondingIPs []data.UniqueIP `bson:"responding_ips"`
			}

			_ = ssn.DB(d.db.GetSelectedDB()).C(d.conf.T.Structure.SNIConnTable).Pipe(sniconnFindQuery).AllowDiskUse().One(&res)

			// Check for errors and parse results
			// this is here because it will still return an empty document even if there are no results
			if res.Count > 0 {
				analysisInput := dissectorResults{
					Hosts:           datum,
					RespondingIPs:   res.RespondingIPs,
					ConnectionCount: res.Count,
					TotalBytes:      res.TBytes,
				}

				// check if sniconn has become a strobe
				if analysisInput.ConnectionCount > d.connLimit {
					d.dissectedCallback(analysisInput)
				} else { // otherwise, parse timestamps and orig ip bytes
					analysisInput.TsList = res.Ts
					analysisInput.TsListFull = res.TsFull
					analysisInput.OrigBytesList = res.Bytes
					// the analysis worker requires that we have over UNIQUE 3 timestamps
					// we drop the input here since it is the earliest place in the pipeline to do so
					if len(analysisInput.TsList) > 3 {
						d.dissectedCallback(analysisInput)
					}
				}
			}
		}
		d.dissectWg.Done()
	}()
}

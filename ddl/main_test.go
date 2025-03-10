// Copyright 2022 PingCAP, Inc.
//
// Licensed under the Apache License, Version 2.0 (the "License");
// you may not use this file except in compliance with the License.
// You may obtain a copy of the License at
//
//     http://www.apache.org/licenses/LICENSE-2.0
//
// Unless required by applicable law or agreed to in writing, software
// distributed under the License is distributed on an "AS IS" BASIS,
// WITHOUT WARRANTIES OR CONDITIONS OF ANY KIND, either express or implied.
// See the License for the specific language governing permissions and
// limitations under the License.

package ddl_test

import (
	"context"
	"fmt"
	"os"
	"testing"
	"time"

	"github.com/pingcap/tidb/config"
	"github.com/pingcap/tidb/ddl"
	"github.com/pingcap/tidb/domain"
	"github.com/pingcap/tidb/domain/infosync"
	"github.com/pingcap/tidb/meta/autoid"
	"github.com/pingcap/tidb/parser/model"
	"github.com/pingcap/tidb/sessionctx"
	"github.com/pingcap/tidb/testkit"
	"github.com/pingcap/tidb/util/testbridge"
	"github.com/tikv/client-go/v2/tikv"
	"go.uber.org/goleak"
)

func TestMain(m *testing.M) {
	testbridge.SetupForCommonTest()
	tikv.EnableFailpoints()

	domain.SchemaOutOfDateRetryInterval.Store(50 * time.Millisecond)
	domain.SchemaOutOfDateRetryTimes.Store(50)

	autoid.SetStep(5000)
	ddl.ReorgWaitTimeout = 30 * time.Millisecond
	ddl.SetBatchInsertDeleteRangeSize(2)

	config.UpdateGlobal(func(conf *config.Config) {
		// Test for table lock.
		conf.EnableTableLock = true
		conf.Log.SlowThreshold = 10000
		conf.TiKVClient.AsyncCommit.SafeWindow = 0
		conf.TiKVClient.AsyncCommit.AllowedClockDrift = 0
		conf.Experimental.AllowsExpressionIndex = true
	})

	_, err := infosync.GlobalInfoSyncerInit(context.Background(), "t", func() uint64 { return 1 }, nil, true)
	if err != nil {
		_, _ = fmt.Fprintf(os.Stderr, "ddl: infosync.GlobalInfoSyncerInit: %v\n", err)
		os.Exit(1)
	}

	opts := []goleak.Option{
		goleak.IgnoreTopFunction("go.etcd.io/etcd/client/pkg/v3/logutil.(*MergeLogger).outputLoop"),
		goleak.IgnoreTopFunction("go.opencensus.io/stats/view.(*worker).start"),
	}

	goleak.VerifyTestMain(m, opts...)
}

func wrapJobIDExtCallback(oldCallback ddl.Callback) *testDDLJobIDCallback {
	return &testDDLJobIDCallback{
		Callback: oldCallback,
		jobID:    0,
	}
}

func setupJobIDExtCallback(ctx sessionctx.Context) (jobExt *testDDLJobIDCallback, tearDown func()) {
	dom := domain.GetDomain(ctx)
	originHook := dom.DDL().GetHook()
	jobIDExt := wrapJobIDExtCallback(originHook)
	dom.DDL().SetHook(jobIDExt)
	return jobIDExt, func() {
		dom.DDL().SetHook(originHook)
	}
}

func checkDelRangeAdded(tk *testkit.TestKit, jobID int64, elemID int64) {
	query := `select sum(cnt) from
	(select count(1) cnt from mysql.gc_delete_range where job_id = ? and element_id = ? union
	select count(1) cnt from mysql.gc_delete_range_done where job_id = ? and element_id = ?) as gdr;`
	tk.MustQuery(query, jobID, elemID, jobID, elemID).Check(testkit.Rows("1"))
}

type testDDLJobIDCallback struct {
	ddl.Callback
	jobID int64
}

func (t *testDDLJobIDCallback) OnJobUpdated(job *model.Job) {
	if t.jobID == 0 {
		t.jobID = job.ID
	}
	if t.Callback != nil {
		t.Callback.OnJobUpdated(job)
	}
}

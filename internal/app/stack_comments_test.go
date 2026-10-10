package app

import (
	"encoding/json"
	"os"
	"path/filepath"
	"strings"
	"testing"
)

func TestPRsPreservesBodiesAndUpdatesStackComments(t *testing.T) {
	t.Parallel()
	a := trainSetup(t)
	a.Cfg.Train.Output = "single"
	_, calls := originWithGh(t,a)
	landTask(t,a,"t1","one",map[string]string{"one.txt":"one\n"})
	second:=landTask(t,a,"t2","two",map[string]string{"two.txt":"two\n"})
	authorBody:="Author description\n- [ ] checklist"
	must(t,a.Store.SetField(second.ID,"summary",authorBody))
	_,err:=a.PRs(); must(t,err)
	for _, call:=range calls() {
		if strings.HasPrefix(call,"pr edit ") && strings.Contains(call,"--body") { t.Fatalf("author body overwritten: %s",call) }
	}
	second=mustTask(t,a,second.ID)
	text:=lastStackComment(t,second.PR)
	if !strings.Contains(text,"**Stack**") || !strings.Contains(text,"Base: `"+mustTask(t,a,"t1").Branch+"`") {t.Fatalf("wrong stack comment: %s",text)}
	for _,id:=range []string{"t1","t2"} {
		task:=mustTask(t,a,id)
		b,err:=json.Marshal(map[string]any{"state":"OPEN","baseRefName":"","comments":[]map[string]string{{"id":"IC_"+id,"body":"<!-- saddle:stack -->\nold"}}})
		must(t,err)
		must(t,os.WriteFile(filepath.Join(fakeGHDir(t),"view-"+filepath.Base(task.PR)),b,0o644))
	}
	before:=len(calls())
	_,err=a.PRs(); must(t,err)
	updates:=0
	for _, call:=range calls()[before:] {
		if strings.HasPrefix(call,"pr comment ") {t.Fatalf("duplicate stack comment: %s",call)}
		if strings.Contains(call,"updateIssueComment") {updates++}
		if strings.HasPrefix(call,"pr edit ") && strings.Contains(call,"--body") {t.Fatal("edited author's revised body")}
	}
	if updates!=2 {t.Fatalf("comment updates=%d, want 2",updates)}
}

func lastStackComment(t *testing.T, pr string) string {
	t.Helper()
	b,err:=os.ReadFile(filepath.Join(fakeGHDir(t),"gh.log"));must(t,err)
	log:=string(b)
	i:=strings.LastIndex(log,"pr comment "+pr+" --body ")
	if i<0 {return ""}
	text:=log[i:]
	if j:=strings.Index(text,"\nBase: `");j>=0 {if end:=strings.Index(text[j+1:],"\n");end>=0 {text=text[:j+1+end]}}
	return text
}

package app

import (
	"strings"
	"testing"

	"github.com/brandonapol/saddle/internal/config"
)

func TestPrepublishSetupRunsInEachCheckedOutLayer(t *testing.T) {
	t.Parallel()
	a:=trainSetup(t)
	_,_=originWithGh(t,a)
	landTask(t,a,"t1","one",map[string]string{"one.txt":"one\n"})
	landTask(t,a,"t2","two",map[string]string{"two.txt":"two\n"})
	write(t,a.Root,".saddle/config.toml","[train]\nprepublish.setup = \"mkdir -p .dart_tool; echo ready > .dart_tool/package_config.json\"\nprepublish.cmd = \"test -f .dart_tool/package_config.json\"\n")
	cfg,err:=config.Load(a.Root);must(t,err)
	a.Cfg.Train.Prepublish=cfg.Train.Prepublish
	_,err=a.PRs(); if err!=nil {t.Fatalf("gate without dependency setup: %v",err)}
}

func TestIdenticalPrepublishFailuresAreSharedEnvironment(t *testing.T) {
	t.Parallel()
	a:=trainSetup(t)
	_,_=originWithGh(t,a)
	landTask(t,a,"t1","one",map[string]string{"one.txt":"one\n"})
	landTask(t,a,"t2","two",map[string]string{"two.txt":"two\n"})
	a.Cfg.Train.Prepublish.Cmd="echo 'dependency language configuration missing'; false"
	_,err:=a.PRs()
	if err==nil || !strings.Contains(err.Error(),"environment") || !strings.Contains(err.Error(),"prgate") {t.Fatalf("missing shared environment diagnosis and directory: %v",err)}
	s,err:=a.Gate();must(t,err)
	if len(s.Red)!=0 {t.Fatalf("identical failures blamed on layers: %+v",s.Red)}
}

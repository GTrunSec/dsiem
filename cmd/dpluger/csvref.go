package main

import (
	"os"
	"path"
	"sort"
	"strconv"

	"github.com/dogenzaka/tsv"
)

type pluginSIDRef struct {
	Name  string `tsv:"plugin"`
	ID    int    `tsv:"id"`
	SID   int    `tsv:"sid"`
	Title string `tsv:"title"`
}

type csvRef struct {
	sids  map[int]pluginSIDRef
	fname string
}

func (c *csvRef) setFilename(pluginName string, confFile string) {
	dir := path.Dir(confFile)
	c.fname = path.Join(dir, pluginName+"_plugin-sids.tsv")
	return
}

func (c *csvRef) init(pluginName string, confFile string) {
	c.sids = make(map[int]pluginSIDRef)
	c.setFilename(pluginName, confFile)
	f, err := os.OpenFile(c.fname, os.O_RDONLY, 0600)
	if err != nil {
		return
	}
	defer f.Close()
	ref := pluginSIDRef{}
	parser, _ := tsv.NewParser(f, &ref)
	for {
		eof, err := parser.Next()
		if err != nil {
			continue
		}
		c.sids[ref.SID] = ref
		if eof {
			break
		}
	}
}

func (c *csvRef) upsert(pluginName string, pluginID int,
	pluginSID *int, pluginTitle string) (shouldIncreaseID bool) {

	// First check the title, exit if already exist
	tKey := 0
	for k, v := range c.sids {
		if v.Title == pluginTitle {
			tKey = k
			break
		}
	}
	if tKey != 0 {
		// should increase new SID number if tKey == pluginSID by coincidence
		if tKey == *pluginSID {
			return true
		}
		return false
	}
	// here title doesnt yet exist so we add it

	// first find available SID
	for {
		_, used := c.sids[*pluginSID]
		if !used {
			break
		}
		*pluginSID++
	}
	r := pluginSIDRef{
		Name:  pluginName,
		SID:   *pluginSID,
		ID:    pluginID,
		Title: pluginTitle,
	}
	c.sids[*pluginSID] = r
	return true
}

func (c csvRef) save() error {
	f, err := os.OpenFile(c.fname, os.O_TRUNC|os.O_CREATE|os.O_WRONLY, 0600)
	if err != nil {
		return err
	}
	defer f.Close()
	if _, err := f.WriteString("plugin\tid\tsid\ttitle\n"); err != nil {
		return err
	}
	// use slice to get a sorted keys, ikr
	var keys []int
	for k := range c.sids {
		keys = append(keys, k)
	}
	sort.Ints(keys)
	for _, k := range keys {
		v := c.sids[k]
		if _, err := f.WriteString(v.Name + "\t" +
			strconv.Itoa(v.ID) + "\t" + strconv.Itoa(v.SID) + "\t" +
			v.Title + "\n"); err != nil {
			return err
		}
	}
	return nil
}
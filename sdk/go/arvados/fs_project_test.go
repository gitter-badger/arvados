// Copyright (C) The Arvados Authors. All rights reserved.
//
// SPDX-License-Identifier: Apache-2.0

package arvados

import (
	"bytes"
	"encoding/json"
	"io"
	"os"

	"git.curoverse.com/arvados.git/sdk/go/arvadostest"
	check "gopkg.in/check.v1"
)

type spiedRequest struct {
	method string
	path   string
	params map[string]interface{}
}

type spyingClient struct {
	*Client
	calls []spiedRequest
}

func (sc *spyingClient) RequestAndDecode(dst interface{}, method, path string, body io.Reader, params interface{}) error {
	var paramsCopy map[string]interface{}
	var buf bytes.Buffer
	json.NewEncoder(&buf).Encode(params)
	json.NewDecoder(&buf).Decode(&paramsCopy)
	sc.calls = append(sc.calls, spiedRequest{
		method: method,
		path:   path,
		params: paramsCopy,
	})
	return sc.Client.RequestAndDecode(dst, method, path, body, params)
}

func (s *SiteFSSuite) TestHomeProject(c *check.C) {
	f, err := s.fs.Open("/home")
	c.Assert(err, check.IsNil)
	fis, err := f.Readdir(-1)
	c.Check(len(fis), check.Not(check.Equals), 0)

	ok := false
	for _, fi := range fis {
		c.Check(fi.Name(), check.Not(check.Equals), "")
		if fi.Name() == "A Project" {
			ok = true
		}
	}
	c.Check(ok, check.Equals, true)

	f, err = s.fs.Open("/home/A Project/..")
	c.Assert(err, check.IsNil)
	fi, err := f.Stat()
	c.Check(err, check.IsNil)
	c.Check(fi.IsDir(), check.Equals, true)
	c.Check(fi.Name(), check.Equals, "home")

	f, err = s.fs.Open("/home/A Project/A Subproject")
	c.Check(err, check.IsNil)
	fi, err = f.Stat()
	c.Check(err, check.IsNil)
	c.Check(fi.IsDir(), check.Equals, true)

	for _, nx := range []string{
		"/home/Unrestricted public data",
		"/home/Unrestricted public data/does not exist",
		"/home/A Project/does not exist",
	} {
		c.Log(nx)
		f, err = s.fs.Open(nx)
		c.Check(err, check.NotNil)
		c.Check(os.IsNotExist(err), check.Equals, true)
	}
}

func (s *SiteFSSuite) TestProjectUpdatedByOther(c *check.C) {
	project, err := s.fs.OpenFile("/home/A Project", 0, 0)
	c.Check(err, check.IsNil)

	_, err = s.fs.Open("/home/A Project/oob")
	c.Check(err, check.NotNil)

	oob := Collection{
		Name:      "oob",
		OwnerUUID: arvadostest.AProjectUUID,
	}
	err = s.client.RequestAndDecode(&oob, "POST", "arvados/v1/collections", s.client.UpdateBody(&oob), nil)
	c.Assert(err, check.IsNil)
	defer s.client.RequestAndDecode(nil, "DELETE", "arvados/v1/collections/"+oob.UUID, nil, nil)

	err = project.Sync()
	c.Check(err, check.IsNil)
	f, err := s.fs.Open("/home/A Project/oob")
	c.Assert(err, check.IsNil)
	fi, err := f.Stat()
	c.Check(fi.IsDir(), check.Equals, true)
	f.Close()

	wf, err := s.fs.OpenFile("/home/A Project/oob/test.txt", os.O_CREATE|os.O_RDWR, 0700)
	c.Assert(err, check.IsNil)
	_, err = wf.Write([]byte("hello oob\n"))
	c.Check(err, check.IsNil)
	err = wf.Close()
	c.Check(err, check.IsNil)

	// Delete test.txt behind s.fs's back by updating the
	// collection record with the old (empty) ManifestText.
	err = s.client.RequestAndDecode(nil, "PATCH", "arvados/v1/collections/"+oob.UUID, s.client.UpdateBody(&oob), nil)
	c.Assert(err, check.IsNil)

	err = project.Sync()
	c.Check(err, check.IsNil)
	_, err = s.fs.Open("/home/A Project/oob/test.txt")
	c.Check(err, check.NotNil)
	_, err = s.fs.Open("/home/A Project/oob")
	c.Check(err, check.IsNil)

	err = s.client.RequestAndDecode(nil, "DELETE", "arvados/v1/collections/"+oob.UUID, nil, nil)
	c.Assert(err, check.IsNil)

	err = project.Sync()
	c.Check(err, check.IsNil)
	_, err = s.fs.Open("/home/A Project/oob")
	c.Check(err, check.NotNil)
}

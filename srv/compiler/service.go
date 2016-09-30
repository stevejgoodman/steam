/*
  Copyright (C) 2016 H2O.ai, Inc. <http://h2o.ai/>

  This program is free software: you can redistribute it and/or modify
  it under the terms of the GNU Affero General Public License as
  published by the Free Software Foundation, either version 3 of the
  License, or (at your option) any later version.

  This program is distributed in the hope that it will be useful,
  but WITHOUT ANY WARRANTY; without even the implied warranty of
  MERCHANTABILITY or FITNESS FOR A PARTICULAR PURPOSE.  See the
  GNU Affero General Public License for more details.

  You should have received a copy of the GNU Affero General Public License
  along with this program.  If not, see <http://www.gnu.org/licenses/>.
*/

package compiler

import (
	"bytes"
	"fmt"
	"io"
	"io/ioutil"
	"mime/multipart"
	"net/http"
	"net/url"
	"os"
	"path"
	"strings"

	"github.com/h2oai/steam/lib/fs"
	"github.com/pkg/errors"
)

const (
	ArtifactWar       = "war"
	ArtifactJar       = "jar"
	ArtifactPythonWar = "pywar"
)

type fileType string

const (
	fileTypeJava        fileType = "pojo"
	fileTypeJavaDep              = "jar"
	fileTypeMOJO                 = "mojo"
	fileTypePythonMain           = "python"
	fileTypePythonOther          = "pythonextra"
)

type asset struct {
	path string
	typ  fileType
}

func CompileModel(address, wd string, projectId, modelId int64, modelLogicalName, artifact, kind, packageName string) (string, error) {

	genModelPath := fs.GetGenModelPath(wd, modelId)
	var modelAsset asset
	switch kind {
	case "pojo":
		modelAsset.typ = fileTypeJava
		modelAsset.path = fs.GetJavaModelPath(wd, modelId, modelLogicalName)
	case "mojo":
		modelAsset.typ = fileTypeMOJO
		modelAsset.path = fs.GetMOJOPath(wd, modelId, modelLogicalName)
	}

	var targetFile, slug string

	switch artifact {
	case ArtifactWar:
		targetFile = fs.GetWarFilePath(wd, modelId, modelLogicalName)
		slug = "makewar"
	case ArtifactPythonWar:
		targetFile = fs.GetPythonWarFilePath(wd, modelId, modelLogicalName)
		slug = "makepythonwar"
	case ArtifactJar:
		targetFile = fs.GetModelJarFilePath(wd, modelId, modelLogicalName)
		slug = "compile"
	}

	if _, err := os.Stat(targetFile); os.IsNotExist(err) {
	} else {
		return targetFile, nil
	}

	// ping to check if service is up
	if _, err := http.Get(toUrl(address, "ping")); err != nil {
		return "", errors.Wrap(err, "could not connect to prediction service builder")
	}

	packageName = strings.TrimSpace(packageName)

	var pythonMainFilePath string
	var pythonOtherFilePaths []string

	if artifact == ArtifactPythonWar && len(packageName) > 0 {
		var err error
		pythonMainFilePath, pythonOtherFilePaths, err = getPythonFilePaths(wd, projectId, packageName)
		if err != nil {
			return "", err
		}
	}

	if err := callCompiler(toUrl(address, slug), targetFile, genModelPath, pythonMainFilePath, modelAsset, pythonOtherFilePaths); err != nil {
		return "", errors.Wrap(err, "failed compiler request")
	}

	return targetFile, nil
}

func callCompiler(url, targetFile, javaDepPath, pythonMainFilePath string, modelAsset asset, pythonOtherFilePaths []string) error {
	b := &bytes.Buffer{}
	writer := multipart.NewWriter(b)

	if err := attachFile(writer, modelAsset.path, string(modelAsset.typ)); err != nil {
		return errors.Wrap(err, "failed attaching model")
	}

	if err := attachFile(writer, javaDepPath, fileTypeJavaDep); err != nil {
		return errors.Wrap(err, "failed attaching java dependency")
	}

	if len(pythonMainFilePath) > 0 {
		if err := attachFile(writer, pythonMainFilePath, fileTypePythonMain); err != nil {
			return fmt.Errorf("Failed attaching Python main file to compilation request: %s", err)
		}
		if pythonOtherFilePaths != nil && len(pythonOtherFilePaths) > 0 {
			for _, p := range pythonOtherFilePaths {
				if err := attachFile(writer, p, fileTypePythonOther); err != nil {
					return fmt.Errorf("Failed attaching Python file to compilation request: %s", err)
				}
			}
		}
	}

	ct := writer.FormDataContentType()
	writer.Close()

	res, err := http.Post(url, ct, b)
	if err != nil {
		return fmt.Errorf("Failed making compilation request: %v", err)
	}
	defer res.Body.Close()

	if res.StatusCode != 200 {
		body, err := ioutil.ReadAll(res.Body)
		if err != nil {
			return fmt.Errorf("Failed reading compilation response: %v", err)
		}
		return fmt.Errorf("Failed compiling scoring service: %s / %s", res.Status, string(body))
	}

	dst, err := os.OpenFile(targetFile, os.O_WRONLY|os.O_CREATE, 0666)
	if err != nil {
		return fmt.Errorf("Failed creating compiled artifact %s: %v", targetFile, err)
	}
	defer dst.Close()

	if _, err := io.Copy(dst, res.Body); err != nil {
		return fmt.Errorf("Failed writing compiled artifact %s: %v", targetFile, err)
	}

	return nil
}

func getPythonFilePaths(wd string, projectId int64, packageName string) (string, []string, error) {
	var pythonMainFilePath string
	var pythonOtherFilePaths []string

	packagePath := fs.GetPackagePath(wd, projectId, packageName)

	if !fs.DirExists(packagePath) {
		return "", nil, fmt.Errorf("Package %s does not exist")
	}

	packageAttrsBytes, err := fs.GetPackageAttributes(wd, projectId, packageName)
	if err != nil {
		return "", nil, fmt.Errorf("Failed reading package attributes: %s", err)
	}

	packageAttrs, err := fs.JsonToMap(packageAttrsBytes)
	if err != nil {
		return "", nil, fmt.Errorf("Failed parsing package attributes: %s", err)
	}

	pythonMain, ok := packageAttrs["main"]
	if !ok {
		return "", nil, fmt.Errorf("Failed determining Python main file from package attributes")
	}

	packageFileList, err := fs.ListFiles(packagePath)
	if err != nil {
		return "", nil, fmt.Errorf("Failed reading package file list: %s", err)
	}

	// Filter .py files; separate ancillary files from the main one.
	pythonOtherFilePaths = make([]string, 0)
	for _, f := range packageFileList {
		if strings.ToLower(path.Ext(f)) == ".py" {
			p := path.Join(packagePath, f)
			if f == pythonMain {
				pythonMainFilePath = p
			} else {
				pythonOtherFilePaths = append(pythonOtherFilePaths, p)
			}
		}
	}

	if len(pythonMainFilePath) == 0 {
		return "", nil, fmt.Errorf("Failed locating Python main file in package file listing")
	}

	return pythonMainFilePath, pythonOtherFilePaths, nil
}

func toUrl(address, slug string) string {
	return (&url.URL{Scheme: "http", Host: address, Path: slug}).String()
}

func attachFile(w *multipart.Writer, filePath, fileType string) error {
	dst, err := w.CreateFormFile(fileType, path.Base(filePath))
	if err != nil {
		return fmt.Errorf("Failed creating form attachment: %s", err)
	}
	src, err := os.Open(filePath)
	if err != nil {
		return fmt.Errorf("Failed opening file for attachment: %s", err)
	}
	if _, err := io.Copy(dst, src); err != nil {
		return fmt.Errorf("Failed attaching file: %s", err)
	}

	return nil
}

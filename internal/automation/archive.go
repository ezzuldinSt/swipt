package automation

import (
	"archive/zip"
	"io"
	"io/fs"
	"os"
	"path"
	"slices"
	"strconv"
	"strings"

	E "github.com/sagernet/sing/common/exceptions"
)

type ArchiveFile struct {
	*zip.File
	Name string
}

type Archive struct {
	Manifest Manifest
	Files    []ArchiveFile
}

func ReadArchive(reader *zip.Reader) (Archive, error) {
	var archive Archive
	var manifestFile *zip.File
	for _, file := range reader.File {
		name := file.Name
		if file.FileInfo().IsDir() || name != ManifestFile && name != IconFile && !strings.HasPrefix(name, SourceFolder+"/") && !strings.HasPrefix(name, AssetsFolder+"/") {
			continue
		}
		if !fs.ValidPath(name) || strings.Contains(name, `\`) {
			return Archive{}, E.New("the archive has an unsafe path ", strconv.Quote(file.Name))
		}
		if name == ManifestFile {
			manifestFile = file
		}
		archive.Files = append(archive.Files, ArchiveFile{file, name})
	}
	if manifestFile == nil {
		return Archive{}, E.New("the archive has no ", ManifestFile)
	}
	data, err := readZipFile(manifestFile)
	if err != nil {
		return Archive{}, err
	}
	manifest, err := ParseManifest(data)
	if err != nil {
		return Archive{}, err
	}
	if err = CheckID(manifest.ID); err != nil {
		return Archive{}, err
	}
	has := func(entry string) bool {
		return slices.ContainsFunc(archive.Files, func(file ArchiveFile) bool { return file.Name == path.Join(SourceFolder, entry) })
	}
	extension := manifest.Service || len(manifest.Actions) > 0
	switch {
	case manifest.Interface && !has(InterfaceFile):
		return Archive{}, E.New("the manifest declares an interface, but the archive has no ", path.Join(SourceFolder, InterfaceFile))
	case extension && !has(ExtensionFile):
		return Archive{}, E.New("the manifest declares a service or actions, but the archive has no ", path.Join(SourceFolder, ExtensionFile))
	case !manifest.Interface && !extension:
		return Archive{}, E.New("the manifest declares neither an interface nor a service or actions")
	}
	archive.Manifest = manifest
	return archive, nil
}

func readZipFile(file *zip.File) ([]byte, error) {
	reader, err := file.Open()
	if err != nil {
		return nil, err
	}
	defer reader.Close()
	return io.ReadAll(reader)
}

func (a Archive) extract(directory string) error {
	root, err := os.OpenRoot(directory)
	if err != nil {
		return err
	}
	defer root.Close()
	for _, file := range a.Files {
		if err = extractFile(root, file); err != nil {
			return E.Cause(err, "extract ", file.File.Name)
		}
	}
	return nil
}

func extractFile(root *os.Root, file ArchiveFile) error {
	if folder := path.Dir(file.Name); folder != "." {
		if err := root.MkdirAll(folder, 0o755); err != nil {
			return err
		}
	}
	reader, err := file.Open()
	if err != nil {
		return err
	}
	defer reader.Close()
	writer, err := root.Create(file.Name)
	if err != nil {
		return err
	}
	_, err = io.Copy(writer, reader)
	return E.Errors(err, writer.Close())
}

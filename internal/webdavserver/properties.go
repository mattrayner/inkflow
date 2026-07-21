package webdavserver

import (
	"bytes"
	"encoding/xml"
	"errors"
	"fmt"
	"io"
	"mime"
	"net/http"
	"os"
	"path"
	"path/filepath"
	"sort"
	"strings"
	"time"

	"inkflow/internal/state"
)

const davNamespace = "DAV:"

type propertyName struct {
	Namespace string
	Local     string
}

func (n propertyName) key() string { return n.Namespace + "\x00" + n.Local }

type davProperty struct {
	name  propertyName
	value string
	raw   bool
}

func (p davProperty) MarshalXML(encoder *xml.Encoder, _ xml.StartElement) error {
	start := xml.StartElement{}
	if p.name.Namespace == davNamespace {
		start.Name.Local = "D:" + p.name.Local
	} else {
		start.Name.Local = p.name.Local
		if p.name.Namespace != "" {
			start.Attr = append(start.Attr, xml.Attr{Name: xml.Name{Local: "xmlns"}, Value: p.name.Namespace})
		}
	}
	if err := encoder.EncodeToken(start); err != nil {
		return err
	}
	if p.value != "" {
		if !p.raw {
			if err := encoder.EncodeToken(xml.CharData(p.value)); err != nil {
				return err
			}
		} else {
			decoder := xml.NewDecoder(strings.NewReader(p.value))
			for {
				token, err := decoder.Token()
				if err == io.EOF {
					break
				}
				if err != nil {
					return err
				}
				if err := encoder.EncodeToken(token); err != nil {
					return err
				}
			}
		}
	}
	return encoder.EncodeToken(start.End())
}

type propfindMode uint8

const (
	propfindAll propfindMode = iota
	propfindNames
	propfindNamed
)

type propfindSelection struct {
	mode  propfindMode
	names []propertyName
}

type patchOperation struct {
	property state.DeadProperty
	remove   bool
}

func parsePropfind(body io.Reader) (propfindSelection, error) {
	data, err := readXMLBody(body)
	if err != nil {
		return propfindSelection{}, err
	}
	if len(bytes.TrimSpace(data)) == 0 {
		return propfindSelection{mode: propfindAll}, nil
	}
	decoder := xml.NewDecoder(bytes.NewReader(data))
	start, err := nextStart(decoder)
	if err != nil {
		return propfindSelection{}, err
	}
	if start.Name.Space != davNamespace || start.Name.Local != "propfind" {
		return propfindSelection{}, fmt.Errorf("expected DAV:propfind root element")
	}
	selection := propfindSelection{mode: propfindAll}
	seen := false
	for {
		token, err := decoder.Token()
		if err == io.EOF {
			return propfindSelection{}, fmt.Errorf("unterminated propfind")
		}
		if err != nil {
			return propfindSelection{}, err
		}
		switch token := token.(type) {
		case xml.StartElement:
			if seen || token.Name.Space != davNamespace {
				return propfindSelection{}, fmt.Errorf("invalid propfind selection")
			}
			seen = true
			switch token.Name.Local {
			case "allprop":
				selection.mode = propfindAll
				if err := decoder.Skip(); err != nil {
					return propfindSelection{}, err
				}
			case "propname":
				selection.mode = propfindNames
				if err := decoder.Skip(); err != nil {
					return propfindSelection{}, err
				}
			case "prop":
				selection.mode = propfindNamed
				names, err := parsePropertyNames(decoder)
				if err != nil {
					return propfindSelection{}, err
				}
				selection.names = names
			default:
				return propfindSelection{}, fmt.Errorf("unsupported propfind selection")
			}
		case xml.EndElement:
			if token.Name == start.Name {
				if !seen {
					return propfindSelection{}, fmt.Errorf("missing propfind selection")
				}
				return selection, nil
			}
		}
	}
}

func parseProppatch(body io.Reader) ([]patchOperation, error) {
	data, err := readXMLBody(body)
	if err != nil {
		return nil, err
	}
	decoder := xml.NewDecoder(bytes.NewReader(data))
	start, err := nextStart(decoder)
	if err != nil {
		return nil, err
	}
	if start.Name.Space != davNamespace || start.Name.Local != "propertyupdate" {
		return nil, fmt.Errorf("expected DAV:propertyupdate root element")
	}
	var operations []patchOperation
	for {
		token, err := decoder.Token()
		if err == io.EOF {
			return nil, fmt.Errorf("unterminated propertyupdate")
		}
		if err != nil {
			return nil, err
		}
		switch token := token.(type) {
		case xml.StartElement:
			if token.Name.Space != davNamespace || (token.Name.Local != "set" && token.Name.Local != "remove") {
				return nil, fmt.Errorf("invalid propertyupdate operation")
			}
			properties, err := parsePatchProperties(decoder)
			if err != nil {
				return nil, err
			}
			for _, property := range properties {
				operations = append(operations, patchOperation{property: property, remove: token.Name.Local == "remove"})
			}
		case xml.EndElement:
			if token.Name == start.Name {
				if len(operations) == 0 {
					return nil, fmt.Errorf("propertyupdate contains no properties")
				}
				return operations, nil
			}
		}
	}
}

func readXMLBody(body io.Reader) ([]byte, error) {
	const maxPropertyXMLBytes = 1 << 20
	data, err := io.ReadAll(io.LimitReader(body, maxPropertyXMLBytes+1))
	if err != nil {
		return nil, err
	}
	if len(data) > maxPropertyXMLBytes {
		return nil, fmt.Errorf("XML request body too large")
	}
	return data, nil
}

func nextStart(decoder *xml.Decoder) (xml.StartElement, error) {
	for {
		token, err := decoder.Token()
		if err != nil {
			return xml.StartElement{}, err
		}
		if start, ok := token.(xml.StartElement); ok {
			return start, nil
		}
	}
}

func parsePropertyNames(decoder *xml.Decoder) ([]propertyName, error) {
	var names []propertyName
	for {
		token, err := decoder.Token()
		if err != nil {
			return nil, err
		}
		switch token := token.(type) {
		case xml.StartElement:
			names = append(names, propertyName{Namespace: token.Name.Space, Local: token.Name.Local})
			if err := decoder.Skip(); err != nil {
				return nil, err
			}
		case xml.EndElement:
			return names, nil
		}
	}
}

func parsePatchProperties(decoder *xml.Decoder) ([]state.DeadProperty, error) {
	var properties []state.DeadProperty
	for {
		token, err := decoder.Token()
		if err != nil {
			return nil, err
		}
		switch token := token.(type) {
		case xml.StartElement:
			if token.Name.Space != davNamespace || token.Name.Local != "prop" {
				return nil, fmt.Errorf("expected DAV:prop")
			}
			for {
				child, err := decoder.Token()
				if err != nil {
					return nil, err
				}
				switch child := child.(type) {
				case xml.StartElement:
					var value struct {
						Inner string `xml:",innerxml"`
					}
					if err := decoder.DecodeElement(&value, &child); err != nil {
						return nil, err
					}
					properties = append(properties, state.DeadProperty{Namespace: child.Name.Space, Local: child.Name.Local, Value: value.Inner})
				case xml.EndElement:
					goto nextOperation
				}
			}
		case xml.EndElement:
			return properties, nil
		}
	nextOperation:
	}
}

func (s *Server) handleProppatch(w http.ResponseWriter, r *http.Request) {
	if !s.cfg.WebDAV.EnableMutation {
		s.methodNotAllowed(w)
		return
	}
	clean, _, _, err := s.resolveVaultPath(r.URL.EscapedPath())
	if err != nil {
		if errors.Is(err, os.ErrNotExist) {
			http.Error(w, "not found", http.StatusNotFound)
			return
		}
		http.Error(w, "invalid path", http.StatusBadRequest)
		return
	}
	if !s.requireLocks(w, r, clean) {
		return
	}
	operations, err := parseProppatch(r.Body)
	if err != nil {
		http.Error(w, "invalid PROPPATCH body", http.StatusBadRequest)
		return
	}

	statuses := make([]propertyStatus, 0, len(operations))
	valid := true
	for _, operation := range operations {
		property := davProperty{name: propertyName{Namespace: operation.property.Namespace, Local: operation.property.Local}}
		status := http.StatusOK
		if operation.property.Namespace == davNamespace {
			status = http.StatusForbidden
			valid = false
		}
		statuses = append(statuses, propertyStatus{property: property, status: status})
	}
	if !valid {
		for i := range statuses {
			if statuses[i].status == http.StatusOK {
				statuses[i].status = http.StatusFailedDependency
			}
		}
	} else if s.store == nil {
		for i := range statuses {
			statuses[i].status = http.StatusServiceUnavailable
		}
	} else {
		changes := make([]state.DeadPropertyChange, 0, len(operations))
		for _, operation := range operations {
			changes = append(changes, state.DeadPropertyChange{DeadProperty: operation.property, Remove: operation.remove})
		}
		if err := s.store.ApplyDeadPropertyChanges(clean, changes); err != nil {
			s.error("persist WebDAV properties", "path", clean, "err", err)
			for i := range statuses {
				statuses[i].status = http.StatusInternalServerError
			}
		}
	}

	w.Header().Set("Content-Type", `application/xml; charset="utf-8"`)
	w.WriteHeader(http.StatusMultiStatus)
	_ = xml.NewEncoder(w).Encode(multistatus{XMLName: xml.Name{Space: davNamespace, Local: "multistatus"}, XMLNSD: davNamespace, Responses: []propResponse{{Href: escapeHref("/" + clean), Propstats: groupPropertyStatuses(statuses)}}})
}

type propertyStatus struct {
	property davProperty
	status   int
}

func groupPropertyStatuses(statuses []propertyStatus) []propstat {
	groups := make(map[int][]davProperty)
	var order []int
	for _, status := range statuses {
		if _, ok := groups[status.status]; !ok {
			order = append(order, status.status)
		}
		groups[status.status] = append(groups[status.status], status.property)
	}
	propstats := make([]propstat, 0, len(order))
	for _, status := range order {
		propstats = append(propstats, propstat{Prop: prop{Properties: groups[status]}, Status: fmt.Sprintf("HTTP/1.1 %d %s", status, http.StatusText(status))})
	}
	return propstats
}

func (s *Server) responseFor(clean, target string, info interface {
	IsDir() bool
	ModTime() time.Time
	Name() string
	Size() int64
}, selection propfindSelection) (propResponse, error) {
	href := escapeHref("/" + strings.TrimPrefix(clean, "/"))
	if info.IsDir() {
		href = strings.TrimSuffix(href, "/") + "/"
	}
	deadProperties := []state.DeadProperty{}
	if s.store != nil {
		var err error
		deadProperties, err = s.store.GetDeadProperties(clean)
		if err != nil {
			return propResponse{}, err
		}
	}
	properties, missing, err := s.selectedProperties(clean, target, info, deadProperties, selection)
	if err != nil {
		return propResponse{}, err
	}
	statuses := make([]propertyStatus, 0, len(properties)+len(missing))
	for _, property := range properties {
		statuses = append(statuses, propertyStatus{property: property, status: http.StatusOK})
	}
	for _, property := range missing {
		statuses = append(statuses, propertyStatus{property: davProperty{name: property}, status: http.StatusNotFound})
	}
	return propResponse{Href: href, Propstats: groupPropertyStatuses(statuses)}, nil
}

func (s *Server) selectedProperties(clean, target string, info interface {
	IsDir() bool
	ModTime() time.Time
	Name() string
	Size() int64
}, deadProperties []state.DeadProperty, selection propfindSelection) ([]davProperty, []propertyName, error) {
	live, err := s.liveProperties(clean, target, info)
	if err != nil {
		return nil, nil, err
	}
	dead := make(map[string]davProperty, len(deadProperties))
	for _, property := range deadProperties {
		dead[property.Namespace+"\x00"+property.Local] = davProperty{name: propertyName{Namespace: property.Namespace, Local: property.Local}, value: property.Value, raw: true}
	}
	if selection.mode == propfindNamed {
		properties := make([]davProperty, 0, len(selection.names))
		var missing []propertyName
		for _, name := range selection.names {
			if property, ok := live[name.key()]; ok {
				properties = append(properties, property)
			} else if property, ok := dead[name.key()]; ok {
				properties = append(properties, property)
			} else {
				missing = append(missing, name)
			}
		}
		return properties, missing, nil
	}
	properties := make([]davProperty, 0, len(live)+len(dead))
	for _, name := range livePropertyOrder {
		if property, ok := live[name.key()]; ok {
			properties = append(properties, property)
		}
	}
	keys := make([]string, 0, len(dead))
	for key := range dead {
		keys = append(keys, key)
	}
	sort.Strings(keys)
	for _, key := range keys {
		properties = append(properties, dead[key])
	}
	if selection.mode == propfindNames {
		for i := range properties {
			properties[i].value = ""
		}
	}
	return properties, nil, nil
}

var livePropertyOrder = []propertyName{
	{Namespace: davNamespace, Local: "resourcetype"},
	{Namespace: davNamespace, Local: "displayname"},
	{Namespace: davNamespace, Local: "getlastmodified"},
	{Namespace: davNamespace, Local: "getcontentlength"},
	{Namespace: davNamespace, Local: "getcontenttype"},
	{Namespace: davNamespace, Local: "getetag"},
	{Namespace: davNamespace, Local: "creationdate"},
	{Namespace: davNamespace, Local: "supportedlock"},
	{Namespace: davNamespace, Local: "lockdiscovery"},
}

func (s *Server) liveProperties(clean, target string, info interface {
	IsDir() bool
	ModTime() time.Time
	Name() string
	Size() int64
}) (map[string]davProperty, error) {
	properties := make(map[string]davProperty, len(livePropertyOrder))
	add := func(local, value string) {
		properties[propertyName{Namespace: davNamespace, Local: local}.key()] = davProperty{name: propertyName{Namespace: davNamespace, Local: local}, value: value}
	}
	resourceType := ""
	if info.IsDir() {
		resourceType = "<D:collection></D:collection>"
	}
	properties[propertyName{Namespace: davNamespace, Local: "resourcetype"}.key()] = davProperty{name: propertyName{Namespace: davNamespace, Local: "resourcetype"}, value: resourceType, raw: resourceType != ""}
	name := info.Name()
	if clean != "" {
		name = path.Base(clean)
	}
	add("displayname", name)
	add("getlastmodified", info.ModTime().UTC().Format(http.TimeFormat))
	add("creationdate", info.ModTime().UTC().Format(time.RFC3339))
	if s.cfg.WebDAV.EnableLocking && s.store != nil {
		locks, err := s.store.LocksForPath(clean)
		if err != nil {
			return nil, err
		}
		properties[propertyName{Namespace: davNamespace, Local: "supportedlock"}.key()] = davProperty{name: propertyName{Namespace: davNamespace, Local: "supportedlock"}, value: supportedLockValue(), raw: true}
		properties[propertyName{Namespace: davNamespace, Local: "lockdiscovery"}.key()] = davProperty{name: propertyName{Namespace: davNamespace, Local: "lockdiscovery"}, value: lockDiscoveryValue(locks), raw: true}
	}
	if info.IsDir() {
		add("getcontenttype", "httpd/unix-directory")
		return properties, nil
	}
	add("getcontentlength", fmt.Sprintf("%d", info.Size()))
	contentType := mime.TypeByExtension(strings.ToLower(filepath.Ext(target)))
	if contentType == "" {
		contentType = "application/octet-stream"
	}
	add("getcontenttype", contentType)
	etag, err := s.etag(clean, target)
	if err != nil {
		return nil, err
	}
	add("getetag", etag)
	return properties, nil
}

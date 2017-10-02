// Copyright 2017 Northern.tech AS
//
//    Licensed under the Apache License, Version 2.0 (the "License");
//    you may not use this file except in compliance with the License.
//    You may obtain a copy of the License at
//
//        http://www.apache.org/licenses/LICENSE-2.0
//
//    Unless required by applicable law or agreed to in writing, software
//    distributed under the License is distributed on an "AS IS" BASIS,
//    WITHOUT WARRANTIES OR CONDITIONS OF ANY KIND, either express or implied.
//    See the License for the specific language governing permissions and
//    limitations under the License.

package main

import (
	"fmt"
	"strings"
	"time"

	"github.com/godbus/dbus"
	"github.com/pkg/errors"
)

func getDevices(msg *dbus.Signal) ([]string, error) {
	devices := make([]string, 0)
	if strings.HasSuffix(msg.Name, "InterfacesAdded") {
		for _, v := range msg.Body {
			if data, ok := v.(dbus.ObjectPath); ok {
				if strings.Contains(string(data), "block_device") {
					devices = append(devices, string(data))
				}
			}
		}
	}
	return devices, nil
}

func registerDeviceCallback(conn *dbus.Conn, signalC chan *dbus.Signal,
	deviceC chan []string) error {
	call := conn.BusObject().Call("org.freedesktop.DBus.AddMatch", 0,
		"type='signal',path='/org/freedesktop/UDisks2',interface='org.freedesktop.DBus.ObjectManager'")

	if call.Err != nil {
		return errors.Wrap(call.Err, "can not register devices signal")
	}

	conn.Signal(signalC)

	go func() {
		for {
			msg, ok := <-signalC
			if !ok {
				return
			}

			dev, err := getDevices(msg)
			if err != nil {
				return
			}
			deviceC <- dev
		}
	}()

	return nil
}

type MountPoints map[string]string

func haveValidDevices(mounted MountPoints) bool {
	for _, mp := range mounted {
		if mp != "" {
			return true
		}
	}
	return false
}

func unmountFile(conn *dbus.Conn, mountPoints MountPoints) error {
	for dev, mp := range mountPoints {
		if mp != "" {
			obj := conn.Object("org.freedesktop.UDisks2", dbus.ObjectPath(dev))
			call := obj.Call("org.freedesktop.UDisks2.Filesystem.Unmount", 0, map[string]dbus.Variant{})
			if call.Err != nil {
				fmt.Printf("[%s] can not unmount device: %s", dev, call.Err.Error())
			}
		} else {
			obj := conn.Object("org.freedesktop.UDisks2", dbus.ObjectPath(dev))
			call := obj.Call("org.freedesktop.UDisks2.Loop.Delete", 0, map[string]dbus.Variant{})
			if call.Err != nil {
				fmt.Printf("[%s] can not unmount device: %s", dev, call.Err.Error())
			}
		}
	}
	return nil
}

func mountFile(conn *dbus.Conn, fd uintptr) (MountPoints, error) {
	if !conn.SupportsUnixFDs() {
		return nil, errors.New("Connection does not support unix fsd")
	}

	// register the callback so that we can see which devices were mounted
	devicesC := make(chan []string)
	signalC := make(chan *dbus.Signal)
	if err := registerDeviceCallback(conn, signalC, devicesC); err != nil {
		return nil, errors.Wrap(err, "Can not register dbus callback")
	}

	obj := conn.Object("org.freedesktop.UDisks2", "/org/freedesktop/UDisks2/Manager")
	call := obj.Call("org.freedesktop.UDisks2.Manager.LoopSetup", 0, dbus.UnixFD(fd),
		map[string]dbus.Variant{"read-only": dbus.MakeVariant(false)})
	if call.Err != nil {
		return nil, errors.Wrap(call.Err, "can not loop mount device")
	}

	var knownDevice string
	if err := call.Store(&knownDevice); err != nil {
		return nil, errors.Wrap(err, "error getting loop device")
	}

	mountPoints := make(MountPoints, 0)

	// in case we've picked up a file which we can not monut, the loop
	// device still will be used; we need to make sure to release the
	//resource afterwards if this was a case
	mountPoints[knownDevice] = ""

	for {
		select {
		case devices := <-devicesC:
			for _, dev := range devices {
				// the only valid devices will be to ones with 'knownDevice' prefix
				if strings.HasPrefix(dev, knownDevice) {
					mp, err := getMonutPoint(conn, dev)
					if err != nil && err != ErrorNoMountPoints {
						return mountPoints, err
					} else if err == ErrorNoMountPoints {
						continue
					}
					mountPoints[dev] = mp
				}
			}
		case <-time.After(time.Second * 1):
			// it is hard to figure out how long should we wait; just see if 1s is enough
			conn.RemoveSignal(signalC)
			close(signalC)
			return mountPoints, nil
		}
	}
}

func contains(s []string, e string) bool {
	for _, v := range s {
		if v == e {
			return true
		}
	}
	return false
}

var ErrorNoMountPoints = MountError{"device has no mount points"}

type MountError struct {
	msg string
}

func (me MountError) Error() string {
	return me.msg
}

func getMonutPoint(conn *dbus.Conn, device string) (string, error) {

	// we are only interested in monut points here
	prop, err := getDeviceProperties(conn, device, []string{"MountPoints"})
	if err == nil {
		switch t := prop["MountPoints"].(type) {
		case [][]uint8:
			return string(t[0]), nil
		default:
			return "", errors.New("unsupported data type")
		}
	}
	return "", ErrorNoMountPoints
}

func getDeviceProperties(conn *dbus.Conn, device string,
	props []string) (map[string]interface{}, error) {

	prop := make(map[string]dbus.Variant)
	requested := make(map[string]interface{}, 0)
	obj := conn.Object("org.freedesktop.UDisks2", dbus.ObjectPath(device))
	call := obj.Call("org.freedesktop.DBus.Properties.GetAll", 0, "org.freedesktop.UDisks2.Filesystem")
	if call.Err != nil {
		return nil, errors.Wrapf(call.Err, "can not get device [%s] properties", device)
	}
	if err := call.Store(prop); err != nil {
		return nil, errors.Wrapf(err, "can not store device [%s] properties", device)
	}

	for k, v := range prop {
		if contains(props, k) {
			requested[k] = v.Value()
		}
	}
	return requested, nil
}

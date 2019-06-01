/*  D3pixelbot - Custom client, recorder and bot for pixel drawing games
    Copyright (C) 2019  David Vogel

    This program is free software: you can redistribute it and/or modify
    it under the terms of the GNU General Public License as published by
    the Free Software Foundation, either version 3 of the License, or
    (at your option) any later version.

    This program is distributed in the hope that it will be useful,
    but WITHOUT ANY WARRANTY; without even the implied warranty of
    MERCHANTABILITY or FITNESS FOR A PARTICULAR PURPOSE.  See the
    GNU General Public License for more details.

    You should have received a copy of the GNU General Public License
	along with this program.  If not, see <https://www.gnu.org/licenses/>.  */

package main

import (
	"encoding/json"
	"fmt"
	"image"
	"image/color"
	"path/filepath"

	"github.com/sciter-sdk/go-sciter"
	"github.com/sciter-sdk/go-sciter/window"
)

// A sciter window, showing a canvas
type sciterCanvas struct {
	connection connection
	canvas     *canvas

	object    *sciter.Value
	cbHandler *sciter.Value // Event handler callback of the window
}

func sciterOpenCanvas(con connection, can *canvas) {
	sca := &sciterCanvas{
		connection: con,
		canvas:     can,
	}

	sciter.SetOption(sciter.SCITER_SET_DEBUG_MODE, 1)
	sciter.SetOption(sciter.SCITER_SET_SCRIPT_RUNTIME_FEATURES, sciter.ALLOW_FILE_IO|sciter.ALLOW_SOCKET_IO|sciter.ALLOW_EVAL|sciter.ALLOW_SYSINFO) // Needed for the inspector to work!

	w, err := window.New(sciter.SW_MAIN|sciter.SW_RESIZEABLE|sciter.SW_TITLEBAR|sciter.SW_CONTROLS|sciter.SW_ENABLE_DEBUG|sciter.SW_GLASSY, sciter.DefaultRect)
	if err != nil {
		log.Fatal(err)
	}

	path, err := filepath.Abs("ui/canvas.htm")
	if err != nil {
		log.Fatal(err)
	}

	if err := w.LoadFile("file://" + path); err != nil {
		log.Fatal(err)
	}

	ok := w.SetOption(sciter.SCITER_SET_DEBUG_MODE, 1)
	if !ok {
		log.Errorf("Failed to set sciter debug mode")
	}

	w.DefineFunction("setEventHandler", func(args ...*sciter.Value) *sciter.Value {
		if len(args) != 2 {
			return sciter.NewValue("Wrong number of parameters")
		}
		obj, cbHandler := args[0].Clone(), args[1].Clone() // Always clone, otherwise those are just references to sciter values and will be invalid if used after return
		if !obj.IsObject() || !cbHandler.IsObjectFunction() {
			return sciter.NewValue("Wrong type of parameters")
		}

		if sca.cbHandler != nil {
			return sciter.NewValue("Callback already set")
		}

		sca.object = obj
		sca.cbHandler = cbHandler
		can.subscribeListener(sca)

		return nil
	})

	rectsChan := make(chan []image.Rectangle, 1)
	defer close(rectsChan)
	go func() {
		for rects := range rectsChan {
			can.registerRects(sca, rects, false)
		}
	}()

	w.DefineFunction("registerRects", func(args ...*sciter.Value) *sciter.Value {
		if len(args) != 1 {
			return sciter.NewValue("Wrong number of parameters")
		}
		jsonRect := args[0] // Clone if value is needed after this function returned
		if !jsonRect.IsObject() {
			return sciter.NewValue("Wrong type of parameters")
		}

		jsonRect.ConvertToString(sciter.CVT_JSON_LITERAL)

		rects := []image.Rectangle{}
		if err := json.Unmarshal([]byte(jsonRect.String()), &rects); err != nil {
			return sciter.NewValue(fmt.Sprintf("Error reading json: %v", err))
		}

		// Write rect into channel, or replace the current one if the goroutine is busy
		select {
		case rectsChan <- rects:
		default:
			select {
			case <-rectsChan:
			default:
			}
			rectsChan <- rects
		}

		return nil
	})

	w.Show()
	w.Run()
}

func (s *sciterCanvas) handleInvalidateAll() error {
	val := sciter.NewValue()
	defer val.Release()
	val.Set("Type", "InvalidateAll")

	s.cbHandler.Invoke(s.object, "[Native Script]", val)

	return nil
}

func (s *sciterCanvas) handleInvalidateRect(rect image.Rectangle) error {
	val := sciter.NewValue()
	defer val.Release()
	val.Set("Type", "InvalidateRect")
	val.Set("X", rect.Min.X)
	val.Set("Y", rect.Min.Y)
	val.Set("Width", rect.Dx())
	val.Set("Height", rect.Dy())

	s.cbHandler.Invoke(s.object, "[Native Script]", val)

	return nil
}

func (s *sciterCanvas) handleSetImage(img image.Image) error {
	val := sciter.NewValue()
	defer val.Release()
	val.Set("Type", "SetImage")
	val.Set("X", img.Bounds().Min.X)
	val.Set("Y", img.Bounds().Min.Y)
	val.Set("Width", img.Bounds().Dx())
	val.Set("Height", img.Bounds().Dy())
	valArray := sciter.NewValue()
	defer valArray.Release()
	valArray.SetBytes(imageToRGBAArray(img))
	val.Set("Array", valArray)

	s.cbHandler.Invoke(s.object, "[Native Script]", val)

	return nil
}

func (s *sciterCanvas) handleSetPixel(pos image.Point, color color.Color) error {
	r, g, b, a := color.RGBA()

	val := sciter.NewValue()
	defer val.Release()
	val.Set("Type", "SetPixel")
	val.Set("X", pos.X)
	val.Set("Y", pos.Y)
	val.Set("R", r)
	val.Set("G", g)
	val.Set("B", b)
	val.Set("A", a)

	s.cbHandler.Invoke(s.object, "[Native Script]", val)

	return nil
}

func (s *sciterCanvas) handleSignalDownload(rect image.Rectangle) error {
	val := sciter.NewValue()
	defer val.Release()
	val.Set("Type", "SignalDownload")
	val.Set("X", rect.Min.X)
	val.Set("Y", rect.Min.Y)
	val.Set("Width", rect.Dx())
	val.Set("Height", rect.Dy())

	s.cbHandler.Invoke(s.object, "[Native Script]", val)

	return nil
}

func (s *sciterCanvas) handleChunksChange(create, remove []image.Rectangle) error {
	jsonData := struct {
		Type           string
		Create, Remove []image.Rectangle
	}{
		"ChunksChange",
		create, remove,
	}

	// TODO: Don't use json as intermediary

	b, err := json.Marshal(jsonData)
	if err == nil {
		val := sciter.NullValue()
		defer val.Release()
		val.ConvertFromString(string(b), sciter.CVT_JSON_LITERAL)
		s.cbHandler.Invoke(s.object, "[Native Script]", val)
	}

	return nil
}

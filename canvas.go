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
	"fmt"
	"image"
	"image/color"
	"image/draw"
	"sync"
	"time"
)

type canvasEventInvalidateAll struct{}

type canvasEventInvalidateRect struct {
	Rect image.Rectangle
}

type canvasEventSetImage struct {
	Image image.Image
}

type canvasEventSetPixel struct {
	Pos   image.Point
	Color color.Color
}

type canvasEventSignalDownload struct {
	Rect image.Rectangle
}

type canvasEventRevalidate struct {
	Rect image.Rectangle
}

type canvasEventListenerSubscribe struct {
	Listener         canvasListener
	UseVirtualChunks bool
}

type canvasEventListenerUnsubscribe struct {
	Listener canvasListener
}

type canvasEventListenerRects struct {
	Listener canvasListener
	Rects    []image.Rectangle
}

type canvasEventSetTime struct {
	Time time.Time
}

type canvasListener interface {
	handleChunksChange(create, remove map[image.Rectangle]int) error

	handleInvalidateAll() error
	handleInvalidateRect(rect image.Rectangle, vcIDs []int) error
	handleSetImage(img image.Image, valid bool, vcIDs []int) error
	handleSetPixel(pos image.Point, color color.Color, vcID int) error
	handleSignalDownload(rect image.Rectangle, vcIDs []int) error
	handleRevalidateRect(rect image.Rectangle, vcIDs []int) error
	handleSetTime(t time.Time) error
}

type canvasListenerState struct {
	Rects                 []image.Rectangle       // Rectangles that the listener needs to be kept up to do date with. The canvas will keep those rectangles in sync with the game
	VirtualChunks         map[image.Rectangle]int // Chunk rectangles with IDs that the listener knows of, only used when UseVirtualChunks is set
	VirtualChunkIDCounter int                     // Counter for new chunk IDs
	UseVirtualChunks      bool                    // True: Let the canvas manage chunks for the listener
}

type canvas struct {
	sync.RWMutex
	Closed      bool
	ClosedMutex sync.RWMutex

	ChunkSize pixelSize
	Origin    image.Point     // Offset of the chunks in pixels. Positive values move the chunks to the top left.
	Rect      image.Rectangle // Valid area of the canvas // TODO: Enforce canvas limit
	Chunks    map[chunkCoordinate]*chunk

	Time time.Time

	EventChan        chan interface{} // Forwards incoming canvasEvent* events to the goroutine
	ChunkRequestChan chan *chunk      // Chunk download requests that go to the game connection
}

func newCanvas(chunkSize pixelSize, origin image.Point, canvasRect image.Rectangle) (*canvas, <-chan *chunk) {
	can := &canvas{
		ChunkSize:        chunkSize,
		Origin:           origin,
		Rect:             canvasRect,
		Chunks:           make(map[chunkCoordinate]*chunk),
		EventChan:        make(chan interface{}), // TODO: Determine optimal chan size (Add waitGroup when channel buffering is enabled!)
		ChunkRequestChan: make(chan *chunk, 500),
	}

	handleChunk := func(chunk *chunk, resetTime bool) {
		switch chunk.getQueryState(resetTime) {
		case chunkDelete:
			can.Lock()
			delete(can.Chunks, can.ChunkSize.getChunkCoord(chunk.Rect.Min, can.Origin))
			can.Unlock()
		case chunkDownload:
			select {
			case can.ChunkRequestChan <- chunk: // Try to send a chunk request to the connection. If it fails --> bleh, retry next time
			default:
			}
		}
	}

	rectQueryChan := make(chan image.Rectangle)

	// Goroutine that handles chunk downloading (Queries the game connection for chunks)
	go func() {
		ticker := time.NewTicker(10 * time.Second)
		defer ticker.Stop()

		for {
			select {
			case rect, ok := <-rectQueryChan:
				if !ok {
					// Close goroutine, as the channel is gone
					return
				}
				chunkRect := can.ChunkSize.getOuterChunkRect(rect, can.Origin)
				chunks, err := can.getChunks(chunkRect, true, true)
				if err == nil {
					for _, chunk := range chunks {
						handleChunk(chunk, true)
					}
				}
			case <-ticker.C: // Query all chunks for state changes regularly
				chunks := can.getAllChunks()
				for _, chunk := range chunks {
					handleChunk(chunk, false) // Handle chunks, but don't reset their timer
				}

			}
		}
	}()

	// Gets the list of virtual chunks that intersect with a given rectangle
	getVirtualChunks := func(state *canvasListenerState, rect image.Rectangle, createNew bool) map[image.Rectangle]int {
		vcs := map[image.Rectangle]int{}
		chunkRect := can.ChunkSize.getOuterChunkRect(rect, can.Origin)
		for iy := chunkRect.Min.Y; iy < chunkRect.Max.Y; iy++ {
			for ix := chunkRect.Min.X; ix < chunkRect.Max.X; ix++ {
				min := image.Point{ix*can.ChunkSize.X - can.Origin.X, iy*can.ChunkSize.Y - can.Origin.X}
				max := min.Add(image.Point{can.ChunkSize.X, can.ChunkSize.Y})

				vc := image.Rectangle{
					Min: min,
					Max: max,
				}

				vcID, ok := state.VirtualChunks[vc] // Get ID from already existing virtual chunk

				if ok {
					vcs[vc] = vcID
					continue
				}

				if createNew {
					vcID = state.VirtualChunkIDCounter
					state.VirtualChunkIDCounter++
					vcs[vc] = vcID
				}
			}
		}
		return vcs
	}

	// Goroutine that handles event broadcasting to listeners
	// It can directly broadcast events from the EventChan, or it can create new events for specific listeners.
	// If requested (by the UseVirtualChunks flag) the goroutine will handle all the creation and deletion of (virtual) chunks for the listener.
	go func() {
		ticker := time.NewTicker(1 * time.Minute)
		defer ticker.Stop()
		listeners := map[canvasListener]*canvasListenerState{} // Events get forwarded to these listeners
		defer close(rectQueryChan)

		for {
			select {
			case event, ok := <-can.EventChan:
				if !ok {
					// Close goroutine, as the channel is gone
					log.Trace("Canvas event broadcaster closed")
					return
				}
				switch event := event.(type) {
				case canvasEventSetPixel:
					//log.Tracef("pixel %v\n", event.Pos)
					for listener, state := range listeners {
						if !state.UseVirtualChunks {
							listener.handleSetPixel(event.Pos, event.Color, 0)
							continue
						}
						vcs := getVirtualChunks(state, image.Rectangle{event.Pos, event.Pos.Add(image.Point{1, 1})}, false)
						for _, vc := range vcs { // Assume that at most one virtual chunk is returned
							//log.Tracef("pixel %v at vcID %v\n", event.Pos, vc)
							listener.handleSetPixel(event.Pos, event.Color, vc)
							break
						}
					}
				case canvasEventSetImage:
					for listener, state := range listeners {
						if !state.UseVirtualChunks {
							listener.handleSetImage(event.Image, true, []int{})
							continue
						}
						vcs := getVirtualChunks(state, event.Image.Bounds(), false)
						if len(vcs) > 0 {
							vcsSlice := []int{}
							for _, vc := range vcs {
								vcsSlice = append(vcsSlice, vc)
							}
							listener.handleSetImage(event.Image, true, vcsSlice)
						}
					}
				case canvasEventInvalidateRect:
					for listener, state := range listeners {
						if !state.UseVirtualChunks {
							listener.handleInvalidateRect(event.Rect, []int{})
							continue
						}
						vcs := getVirtualChunks(state, event.Rect, false)
						if len(vcs) > 0 {
							vcsSlice := []int{}
							for _, vc := range vcs {
								vcsSlice = append(vcsSlice, vc)
							}
							listener.handleInvalidateRect(event.Rect, vcsSlice)
						}
					}
				case canvasEventInvalidateAll:
					for listener := range listeners {
						listener.handleInvalidateAll()
					}
				case canvasEventRevalidate:
					for listener, state := range listeners {
						if !state.UseVirtualChunks {
							listener.handleRevalidateRect(event.Rect, []int{})
							continue
						}
						vcs := getVirtualChunks(state, event.Rect, false)
						if len(vcs) > 0 {
							vcsSlice := []int{}
							for _, vc := range vcs {
								vcsSlice = append(vcsSlice, vc)
							}
							listener.handleRevalidateRect(event.Rect, vcsSlice)
						}
					}
				case canvasEventSignalDownload:
					for listener, state := range listeners {
						if !state.UseVirtualChunks {
							listener.handleSignalDownload(event.Rect, []int{})
							continue
						}
						vcs := getVirtualChunks(state, event.Rect, false)
						if len(vcs) > 0 {
							vcsSlice := []int{}
							for _, vc := range vcs {
								vcsSlice = append(vcsSlice, vc)
							}
							listener.handleSignalDownload(event.Rect, vcsSlice)
						}
					}
				case canvasEventSetTime:
					for listener := range listeners {
						listener.handleSetTime(event.Time)
					}
				case canvasEventListenerSubscribe:
					//log.Tracef("Listener %v subscribed", event.Listener)
					listeners[event.Listener] = &canvasListenerState{
						UseVirtualChunks:      event.UseVirtualChunks,
						VirtualChunkIDCounter: 1,
					}

					// If the canvas doesn't handle the listeners chunks, just send all chunks for initialization
					if !event.UseVirtualChunks {
						chunks := can.getAllChunks()
						for _, chunk := range chunks {
							img, valid, _, err := chunk.getImageCopy(false)
							if err == nil {
								event.Listener.handleSetImage(img, valid, []int{})
							}
						}
					}

					t, err := can.getTime()
					if err == nil {
						event.Listener.handleSetTime(t)
					}

				case canvasEventListenerUnsubscribe:
					//log.Tracef("Listener %v unsubscribed", event.Listener)
					delete(listeners, event.Listener)
				case canvasEventListenerRects:
					state, ok := listeners[event.Listener]
					if ok {
						//log.Tracef("Listener %v changed rects to %v", event.Listener, event.Rects)

						state.Rects = event.Rects

						// Make download query for rects
						for _, rect := range state.Rects {
							go func(rect image.Rectangle) { rectQueryChan <- rect }(rect) // Async download request
						}

						if !state.UseVirtualChunks {
							break
						}

						// Get or create chunk rects that are intersecting with the listener rectangles
						neededChunks := map[image.Rectangle]int{}
						for _, rect := range state.Rects {
							tempChunks := getVirtualChunks(state, rect, true)
							for k, v := range tempChunks {
								neededChunks[k] = v
							}
						}

						// Handle chunk rects, that are missing on the listeners side
						createChunks := map[image.Rectangle]int{}
						for k, v := range neededChunks {
							if _, ok := state.VirtualChunks[k]; !ok {
								createChunks[k] = v
							}
						}

						// Handle chunk rects, that are not needed anymore on the listeners side
						removeChunks := map[image.Rectangle]int{}
						for k, v := range state.VirtualChunks {
							if _, ok := neededChunks[k]; !ok {
								removeChunks[k] = v
							}
						}

						state.VirtualChunks = neededChunks

						if len(createChunks) > 0 || len(removeChunks) > 0 {
							event.Listener.handleChunksChange(createChunks, removeChunks)
						}

						// Additionally send images for the new chunks if possible
						for rect, id := range createChunks {
							chunkCoord := can.ChunkSize.getChunkCoord(rect.Min, can.Origin)
							chunk, err := can.getChunk(chunkCoord, false)
							if err == nil {
								img, valid, _, err := chunk.getImageCopy(false)
								if err == nil {
									event.Listener.handleSetImage(img, valid, []int{id})
								}
							}
						}

					}
				default:
					log.Panicf("Unknown event occurred: %T", event)
				}
			case <-ticker.C: // Query all rects every minute
				for _, state := range listeners {
					for _, rect := range state.Rects {
						go func(rect image.Rectangle) { rectQueryChan <- rect }(rect) // Async download request
					}
				}
			}
		}
	}()

	return can, can.ChunkRequestChan
}

// Subscribes a listener to canvas events.
//
// If useVirtualChunks is true, the canvas will manage chunks for the listener:
// - It will send register virtual chunk events with a list of new chunks for the listener
// - It will send unregister virtual chunk events with a list of chunks to be deleted on the listener side
// - Only events that intersect those registered virtual chunks will be sent to the listener
//
// If that flag is false, the canvas will send all events to the listener.
// Furthermore it will send the images of all known chunks on subscription.
func (can *canvas) subscribeListener(l canvasListener, useVirtualChunks bool) error {
	can.ClosedMutex.RLock()
	defer can.ClosedMutex.RUnlock()
	if can.Closed {
		return fmt.Errorf("Canvas is closed")
	}

	// Forward event to broadcaster goroutine, even if there isn't a chunk.
	can.EventChan <- canvasEventListenerSubscribe{
		Listener:         l,
		UseVirtualChunks: useVirtualChunks,
	}

	return nil
}

func (can *canvas) unsubscribeListener(l canvasListener) error {
	can.ClosedMutex.RLock()
	defer can.ClosedMutex.RUnlock()
	if can.Closed {
		return fmt.Errorf("Canvas is closed")
	}

	// Forward event to broadcaster goroutine, even if there isn't a chunk.
	can.EventChan <- canvasEventListenerUnsubscribe{
		Listener: l,
	}

	return nil
}

// Register a number of rectangles that the listener needs to be kept up to date with.
//
// This function will silently fail if the listener isn't subscribed already.
//
// Don't call this function from the same context that handles events, or it will cause a deadlock.
func (can *canvas) registerRects(l canvasListener, rects []image.Rectangle) error {
	can.ClosedMutex.RLock()
	defer can.ClosedMutex.RUnlock()
	if can.Closed {
		return fmt.Errorf("Canvas is closed")
	}

	// Forward event to broadcaster goroutine, even if there isn't a chunk.
	can.EventChan <- canvasEventListenerRects{
		Listener: l,
		Rects:    rects,
	}

	return nil
}

func (can *canvas) getChunk(coord chunkCoordinate, createIfNonexistent bool) (*chunk, error) {
	if createIfNonexistent {
		can.Lock()
		defer can.Unlock()
	} else {
		can.RLock()
		defer can.RUnlock()
	}

	chunk, ok := can.Chunks[coord]
	if ok {
		return chunk, nil
	}

	if createIfNonexistent {
		min := image.Point{coord.X*can.ChunkSize.X - can.Origin.X, coord.Y*can.ChunkSize.Y - can.Origin.X}
		max := min.Add(image.Point{can.ChunkSize.X, can.ChunkSize.Y})
		chunk := newChunk(
			image.Rectangle{
				Min: min,
				Max: max,
			},
		)

		can.Chunks[coord] = chunk

		return chunk, nil
	}

	return nil, fmt.Errorf("Chunk at %v does not exist", coord)
}

func (can *canvas) getChunks(rect chunkRectangle, createIfNonexistent, ignoreNonexistent bool) ([]*chunk, error) {
	rectTemp := rect.Canon()
	chunks := []*chunk{}

	for iy := rectTemp.Min.Y; iy < rectTemp.Max.Y; iy++ {
		for ix := rectTemp.Min.X; ix < rectTemp.Max.X; ix++ {
			chunk, err := can.getChunk(chunkCoordinate{ix, iy}, createIfNonexistent)
			if err != nil && ignoreNonexistent == false {
				// This assumes that there can only be an error when createIfNonexistent == false
				// So it will never abort while it creates missing chunks
				return nil, fmt.Errorf("Can't get all chunks: %v", err)
			}
			if chunk != nil {
				chunks = append(chunks, chunk)
			}
		}
	}

	return chunks, nil
}

func (can *canvas) getAllChunks() []*chunk {
	can.RLock()
	defer can.RUnlock()

	chunks := []*chunk{}
	for _, chunk := range can.Chunks {
		chunks = append(chunks, chunk)
	}

	return chunks
}

func (can *canvas) getPixel(pos image.Point) (color.Color, error) {
	chunkCoord := can.ChunkSize.getChunkCoord(pos, can.Origin)

	chunk, err := can.getChunk(chunkCoord, false)
	if err != nil {
		return nil, fmt.Errorf("Can't get chunk at %v: %v", pos, err)
	}

	return chunk.getPixel(pos)
}

func (can *canvas) getPixelIndex(pos image.Point) (uint8, error) {
	chunkCoord := can.ChunkSize.getChunkCoord(pos, can.Origin)

	chunk, err := can.getChunk(chunkCoord, false)
	if err != nil {
		return 0, fmt.Errorf("Can't get chunk at %v: %v", pos, err)
	}

	return chunk.getPixelIndex(pos)
}

func (can *canvas) setPixel(pos image.Point, col color.Color) error {
	can.ClosedMutex.RLock()
	defer can.ClosedMutex.RUnlock()
	if can.Closed {
		return fmt.Errorf("Canvas is closed")
	}

	// Forward event to broadcaster goroutine, even if there isn't a chunk. But send it after the chunk has been updated
	defer func() {
		can.EventChan <- canvasEventSetPixel{
			Pos:   pos,
			Color: col,
		}
	}()

	chunkCoord := can.ChunkSize.getChunkCoord(pos, can.Origin)

	chunk, err := can.getChunk(chunkCoord, false)
	if err != nil {
		return fmt.Errorf("Can't get chunk at %v: %v", chunkCoord, err)
	}

	return chunk.setPixel(pos, col)
}

// Will update the canvas with the given image.
// Only chunks that are fully inside the image will be updated.
// Chunks that have their download flag not set, will be ignored.
//
// This will validate the chunks, reset their download flag and replay any pixel events that happened while downloading.
// createIfNonexistent should be set to false normally.
func (can *canvas) setImage(img image.Image, createIfNonexistent, ignoreNonexistent bool) error {
	can.ClosedMutex.RLock()
	defer can.ClosedMutex.RUnlock()
	if can.Closed {
		return fmt.Errorf("Canvas is closed")
	}

	chunkRect := can.ChunkSize.getInnerChunkRect(img.Bounds(), can.Origin)
	chunks, err := can.getChunks(chunkRect, createIfNonexistent, ignoreNonexistent)
	if err != nil {
		return fmt.Errorf("Can't get chunks from rectangle %v: %v", img.Bounds(), err)
	}

	// Copy image, because the chunks will use a subimage of this copy. Otherwise the original image will be edited
	imgCopy, err := copyImage(img)
	if err != nil {
		return fmt.Errorf("Can't copy image at %v: %v", img.Bounds(), err)
	}

	for _, chunk := range chunks {
		resultImg, err := chunk.setImage(imgCopy)
		if err != nil {
			//return fmt.Errorf("Could not draw image at %v: %v", img.Bounds(), err)
			continue
		}
		// Forward event to broadcaster goroutine. It needs to be sent after chunk manipulation to keep everything in sync
		if resultImg != nil {
			can.EventChan <- canvasEventSetImage{
				Image: resultImg,
			}
		} else {
			can.EventChan <- canvasEventRevalidate{
				Rect: chunk.Rect,
			}
		}
	}

	return nil
}

// Get RGBA image of the given rectangle.
// The resulting image can be in an inconsistent state when some chunks change while it's generated.
// But each chunk itself will be consistent.
// To get consistent updates, you should rather subscribe to the canvas change broadcast.
// If ignoreNonexistent is set to true, non existent chunks will be drawn transparent.
// If onlyIfValid is set to true, the function will fail if there are invalid chunks inside.
// If onlyIfValid is set to false, invalid chunks will be drawn transparent or with older data.
func (can *canvas) getImageCopy(rect image.Rectangle, onlyIfValid, ignoreNonexistent bool) (*image.RGBA, error) {
	chunkRect := can.ChunkSize.getOuterChunkRect(rect, can.Origin)
	chunks, err := can.getChunks(chunkRect, false, ignoreNonexistent)
	if err != nil {
		return nil, fmt.Errorf("Can't get chunks from rectangle %v: %v", rect, err)
	}

	img := image.NewRGBA(rect)

	for _, chunk := range chunks {
		imgCopy, _, _, err := chunk.getImageCopy(onlyIfValid)
		if err == nil {
			draw.Draw(img, rect, imgCopy, rect.Min, draw.Over)
		} else if onlyIfValid {
			return nil, fmt.Errorf("Can't get chunk image at %v: %v", chunk.Rect, err)
		}
	}

	return img, nil
}

// Invalidates all chunks the rectangle intersects with.
// This will only affect existing chunks.
//
// This should be used to signal connection loss or something that caused specific chunks to go out of sync.
func (can *canvas) invalidateRect(rect image.Rectangle) error {
	can.ClosedMutex.RLock()
	defer can.ClosedMutex.RUnlock()
	if can.Closed {
		return fmt.Errorf("Canvas is closed")
	}

	// Forward event to broadcaster goroutine. But send after chunks have been invalidated
	defer func() {
		can.EventChan <- canvasEventInvalidateRect{
			Rect: rect,
		}
	}()

	chunkRect := can.ChunkSize.getOuterChunkRect(rect, can.Origin)
	chunks, err := can.getChunks(chunkRect, false, true)
	if err != nil {
		return fmt.Errorf("Can't get chunks from rectangle %v: %v", rect, err)
	}

	for _, chunk := range chunks {
		chunk.invalidateImage()
	}

	return nil
}

// Revalidates chunks to signal that they in sync with the game again.
//
// There is no need to call this function, if SetImage has been used.
func (can *canvas) revalidateRect(rect image.Rectangle) error {
	can.ClosedMutex.RLock()
	defer can.ClosedMutex.RUnlock()
	if can.Closed {
		return fmt.Errorf("Canvas is closed")
	}

	// Forward event to broadcaster goroutine. But send after chunks have been revalidated
	defer func() {
		can.EventChan <- canvasEventRevalidate{
			Rect: rect,
		}
	}()

	chunkRect := can.ChunkSize.getOuterChunkRect(rect, can.Origin)
	chunks, err := can.getChunks(chunkRect, false, true)
	if err != nil {
		return fmt.Errorf("Can't get chunks from rectangle %v: %v", rect, err)
	}

	for _, chunk := range chunks {
		chunk.revalidate()
	}

	return nil
}

// Invalidates all chunks.
// This will only affect existing chunks.
//
// This should be used to signal connection loss.
func (can *canvas) invalidateAll() error {
	can.ClosedMutex.RLock()
	defer can.ClosedMutex.RUnlock()
	if can.Closed {
		return fmt.Errorf("Canvas is closed")
	}

	chunks := can.getAllChunks()

	for _, chunk := range chunks {
		chunk.invalidateImage()
	}

	// Forward event to broadcaster goroutine
	can.EventChan <- canvasEventInvalidateAll{}

	return nil
}

// Sets the current time of the canvas
func (can *canvas) setTime(t time.Time) error {
	can.ClosedMutex.RLock()
	defer can.ClosedMutex.RUnlock()
	if can.Closed {
		return fmt.Errorf("Canvas is closed")
	}

	can.Lock()
	can.Time = t
	can.Unlock()

	// Forward event to broadcaster goroutine
	can.EventChan <- canvasEventSetTime{
		Time: t,
	}

	return nil
}

// Gets the current time of the canvas
func (can *canvas) getTime() (time.Time, error) {
	can.ClosedMutex.RLock()
	defer can.ClosedMutex.RUnlock()
	if can.Closed {
		return time.Time{}, fmt.Errorf("Canvas is closed")
	}

	can.RLock()
	defer can.RUnlock()

	return can.Time, nil
}

// Returns true if the all intersecting chunks are valid and existent
func (can *canvas) isValid(rect image.Rectangle) bool {
	chunkRect := can.ChunkSize.getOuterChunkRect(rect, can.Origin)
	chunks, err := can.getChunks(chunkRect, false, false)
	if err != nil {
		return false
	}

	for _, chunk := range chunks {
		if !chunk.Valid {
			return false
		}
	}

	return true
}

// Signals that the specified rect is being downloaded.
// This will create new chunks if needed.
//
// A list of affected chunks is returned.
//
// This should be used to signal that the download for a specific area has started.
// A chunk that is in the downloading state will queue all pixel events, and will replay them after the download has finished.
// By replaying the pixels, the chunk will always be in sync with the game, even if downloading takes a while.
//
// For some game APIs it may not be necessary, as they send data serially.
// But signalDownload() must always be used, because otherwise the canvas would retrigger the download several times in a row on an invalid chunk.
func (can *canvas) signalDownload(rect image.Rectangle) ([]*chunk, error) {
	can.ClosedMutex.RLock()
	defer can.ClosedMutex.RUnlock()
	if can.Closed {
		return nil, fmt.Errorf("Canvas is closed")
	}

	// Forward event to broadcaster goroutine. But send after chunks have been flagged
	defer func() {
		can.EventChan <- canvasEventSignalDownload{
			Rect: rect,
		}
	}()

	chunkRect := can.ChunkSize.getOuterChunkRect(rect, can.Origin)
	chunks, err := can.getChunks(chunkRect, true, true)
	if err != nil {
		return nil, fmt.Errorf("Can't get chunks from rectangle %v: %v", rect, err)
	}

	downloading := []*chunk{}

	for _, chunk := range chunks {
		if chunk.signalDownload() {
			downloading = append(downloading, chunk)
		}
	}

	return downloading, nil
}

func (can *canvas) Close() {
	can.ClosedMutex.RLock()
	can.Closed = true // Prevent any new events from happening
	can.ClosedMutex.RUnlock()

	close(can.EventChan) // This will stop the goroutine after all events are processed

	return
}

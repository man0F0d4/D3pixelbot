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
	"encoding/binary"
	"fmt"
	"image"
	"image/color"
	"io"
	"io/ioutil"
	"math"
	"os"
	"path/filepath"
	"time"

	gzip "github.com/klauspost/pgzip"
)

type canvasDiskReader struct {
	ShortName string

	Canvas *canvas

	TimeChan chan time.Time // Sends point in time to goroutine
}

func newCanvasDiskReader(shortName string) (connection, *canvas, error) {
	cdr := &canvasDiskReader{
		ShortName: shortName,
		TimeChan:  make(chan time.Time),
	}

	fileDirectory := filepath.Join(".", "recordings", shortName)
	files, err := ioutil.ReadDir(fileDirectory)
	if err != nil {
		return nil, nil, fmt.Errorf("Can't read from %v", fileDirectory)
	}
	if len(files) <= 0 {
		return nil, nil, fmt.Errorf("Can't find any recordings in %v", fileDirectory)
	}

	fileName := filepath.Join(fileDirectory, files[0].Name())
	file, err := os.Open(fileName)
	if err != nil {
		return nil, nil, fmt.Errorf("Can't open recording %v", fileName)
	}
	defer file.Close()

	zipReader, err := gzip.NewReader(file)
	if err != nil {
		return nil, nil, fmt.Errorf("Can't initialize gzip reader for %v: %v", fileName, err)
	}
	defer zipReader.Close()

	parseHeader := func(reader io.Reader) (time.Time, pixelSize, error) {
		var dat struct {
			MagicNumber             uint32
			Version                 uint16 // File format version
			Time                    int64
			ChunkWidth, ChunkHeight uint32
			Reserved                uint16
		}
		err := binary.Read(reader, binary.LittleEndian, &dat)
		if err != nil {
			return time.Time{}, pixelSize{}, fmt.Errorf("Error while reading file: %v", err)
		}

		if dat.MagicNumber != 1128616528 { // ASCII "PREC" in little endian
			return time.Time{}, pixelSize{}, fmt.Errorf("Wrong file format")
		}

		if dat.Version > 1 {
			return time.Time{}, pixelSize{}, fmt.Errorf("Version is newer")
		}

		return time.Unix(0, dat.Time), pixelSize{int(dat.ChunkWidth), int(dat.ChunkHeight)}, nil
	}

	startRecTime, chunkSize, err := parseHeader(zipReader)
	if err != nil {
		return nil, nil, err
	}

	cdr.Canvas, _ = newCanvas(chunkSize, image.Rect(math.MinInt32, math.MinInt32, math.MaxInt32, math.MaxInt32))

	go func() {
		defer log.Tracef("Closed recording goroutine of %v", shortName)

		curTime := <-cdr.TimeChan

	restartLoop:
		for {
			for _, recording := range files {
				// Find first recording that starts at the right time
				restart, close := func() (restart, close bool) {
					if filepath.Ext(recording.Name()) != ".pixrec" {
						return false, false
					}

					// TODO: Jump over files that end before curTime

					/*dateString := strings.TrimSuffix(recording.Name(), filepath.Ext(recording.Name()))
					recTime, err := time.Parse("2006-01-02T150405", dateString)
					if err != nil {
						log.Warnf("Invalid formatted filename %v: %v", recording.Name(), err)
						return false
					}

					if curTime.Before(recTime) {
						break
					}*/

					// Found valid recording, read it
					fileName := filepath.Join(fileDirectory, recording.Name())
					log.Tracef("Open recording %v", fileName)
					file, err := os.Open(fileName)
					if err != nil {
						log.Warnf("Can't open file %v: %v", fileName, err)
						return false, false
					}
					defer file.Close()
					zipReader, err := gzip.NewReader(file)
					if err != nil {
						log.Warnf("Can't decompress %v: %v", fileName, err)
						return false, false
					}
					defer zipReader.Close()

					recTime, _, err := parseHeader(zipReader)
					if err != nil {
						log.Warn(err)
						return false, false
					}

					// Invalidate all on file close
					defer cdr.Canvas.invalidateAll()

					for {
						tempTime, ok := <-cdr.TimeChan
						if !ok {
							return false, true // Close goroutine
						}
						if tempTime.Before(curTime) {
							return true, false // Start from the beginning again
						}
						log.Tracef("Change time from %v to %v (recTime: %v)", curTime, tempTime, recTime)
						curTime = tempTime

						// Do until recTime >= curTime
						for recTime.Before(curTime) {
							// Read and send events
							var dataType uint8
							var binTime int64
							err := binary.Read(zipReader, binary.LittleEndian, &dataType)
							if err != nil {
								log.Warnf("Error while reading file %v: %v", fileName, err)
								return false, false
							}
							err = binary.Read(zipReader, binary.LittleEndian, &binTime)
							if err != nil {
								log.Warnf("Error while reading file %v: %v", fileName, err)
								return false, false
							}
							recTime = time.Unix(0, binTime)

							switch dataType {
							case 10: // SetPixel
								var dat struct {
									X, Y    int32
									R, G, B uint8
								}
								err := binary.Read(zipReader, binary.LittleEndian, &dat)
								if err != nil {
									log.Warnf("Error while reading file %v: %v", fileName, err)
									return false, false
								}
								cdr.Canvas.setPixel(image.Point{int(dat.X), int(dat.Y)}, color.RGBA{dat.R, dat.G, dat.B, 255})

							case 20: // InvalidateRect
								var dat struct {
									MinX, MinY, MaxX, MaxY int32
								}
								err := binary.Read(zipReader, binary.LittleEndian, &dat)
								if err != nil {
									log.Warnf("Error while reading file %v: %v", fileName, err)
									return false, false
								}
								cdr.Canvas.invalidateRect(image.Rect(int(dat.MinX), int(dat.MinY), int(dat.MaxX), int(dat.MaxY)))

							case 21: // InvalidateAll
								cdr.Canvas.invalidateAll()

							case 22: // RevalidateRect
								var dat struct {
									MinX, MinY, MaxX, MaxY int32
								}
								err := binary.Read(zipReader, binary.LittleEndian, &dat)
								if err != nil {
									log.Warnf("Error while reading file %v: %v", fileName, err)
									return false, false
								}
								cdr.Canvas.revalidateRect(image.Rect(int(dat.MinX), int(dat.MinY), int(dat.MaxX), int(dat.MaxY)))

							case 30: // SetImage
								var dat struct {
									X, Y          int32
									Width, Height uint16
									Size          uint32 // Size of the RGB data in bytes
								}
								err := binary.Read(zipReader, binary.LittleEndian, &dat)
								if err != nil {
									log.Warnf("Error while reading file %v: %v", fileName, err)
									return false, false
								}
								imageData := make([]byte, dat.Size)
								err = binary.Read(zipReader, binary.LittleEndian, &imageData)
								if err != nil {
									log.Warnf("Error while reading file %v: %v", fileName, err)
									return false, false
								}
								rect := image.Rect(int(dat.X), int(dat.Y), int(dat.X)+int(dat.Width), int(dat.Y)+int(dat.Height))
								img, err := rgbArrayToImage(imageData, rect)
								if err != nil {
									log.Warnf("Error while reading image from %v: %v", fileName, err)
									return false, false
								}
								cdr.Canvas.signalDownload(rect)
								cdr.Canvas.setImage(img, false, true)

							}
						}
					}
				}()
				if close {
					return
				}
				if restart {
					log.Tracef("Start recording %v from beginning", shortName)
					continue restartLoop
				}
			}
			// Iterated through all files without getting to curTime. Wait for next curTime change
			log.Tracef("Reached end of all recording files of %v", shortName)
			tempTime, ok := <-cdr.TimeChan
			if !ok {
				return // Close goroutine
			}
			curTime = tempTime
		}
	}()

	// Test
	tic := time.NewTicker(10 * time.Millisecond)
	go func() {
		someTime := startRecTime
		for range tic.C {
			someTime = someTime.Add(1 * time.Second)
			cdr.TimeChan <- someTime
		}
	}()

	cdr.TimeChan <- startRecTime

	return cdr, cdr.Canvas, nil
}

func (cdr *canvasDiskReader) getShortName() string {
	return fmt.Sprintf("replay-%v", cdr.ShortName)
}

func (cdr *canvasDiskReader) getName() string {
	return fmt.Sprintf("Replay of %v", cdr.ShortName)
}

func (cdr *canvasDiskReader) getOnlinePlayers() int {
	return 0
}

// Closes the reader and the canvas
func (cdr *canvasDiskReader) Close() {
	// Stop goroutines gracefully
	close(cdr.TimeChan)

	cdr.Canvas.Close()

	return
}
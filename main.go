package main

import (
	"bufio"
	"flag"
	"fmt"
	tf "github.com/tensorflow/tensorflow/tensorflow/go"
	"github.com/tensorflow/tensorflow/tensorflow/go/op"
	"image"
	"image/jpeg"
	"image/png"
	"io/ioutil"
	"log"
	"os"
	"path/filepath"
	"strconv"
	"strings"
)

// Some constants specific to the pre-trained model at:
// https://storage.googleapis.com/download.tensorflow.org/models/inception5h.zip
// - The model was trained after with images scaled to 224x224 pixels.
// - The colors, represented as R, G, B in 1-byte each were converted to
//   float using (value - Mean)/Scale.
const (
	H, W  = 300, 300
	Mean  = float32(117)
	Scale = float32(1)
)

type Chip struct {
	X  int
	Y  int
	Im image.Image
}

type Detect struct {
	Bounds      image.Rectangle
	Class       int
	Chip        *Chip
	Description string
	Confidence  float32
}

func main() {
	modeldir := flag.String("dir", "", "Directory containing the trained model and labels")
	imagefile := flag.String("image", "", "Path of a JPEG-image to extract labels for")
	flag.Parse()
	if *modeldir == "" || *imagefile == "" {
		flag.Usage()
		return
	}
	// Load the serialized GraphDef from a file.
	modelfile, labelsfile, err := modelFiles(*modeldir, "multires")
	if err != nil {
		log.Fatal(err)
	}
	model, err := ioutil.ReadFile(modelfile)
	if err != nil {
		log.Fatal(err)
	}

	// Construct an in-memory graph from the serialized form.
	graph := tf.NewGraph()
	if err := graph.Import(model, ""); err != nil {
		log.Fatal(err)
	}

	// Create a session for inference over graph.
	session, err := tf.NewSession(graph, nil)
	if err != nil {
		log.Fatal(err)
	}
	defer session.Close()

	file, err := os.Open(*imagefile)
	if err != nil {
		log.Fatalf("%v", err)
	}

	im, err := jpeg.Decode(file)
	if err != nil {
		log.Fatalf("%s: %v\n", *imagefile, err)
	}

	wn := im.Bounds().Dx() / W
	hn := im.Bounds().Dy() / H

	chips := make([]Chip, wn*hn)
	for i := 0; i < wn*hn; i++ {
		x := i % wn
		y := i / wn
		w := x * W
		h := y * H

		chip := im.(interface {
			SubImage(r image.Rectangle) image.Image
		}).SubImage(image.Rect(w, h, w+W, h+H))

		outputFile, _ := os.Create(fmt.Sprintf("/tmp/chip-%v.png", i))
		png.Encode(outputFile, chip)
		outputFile.Close()

		chips[i] = Chip{
			x,
			y,
			chip,
		}
	}

	for i, chip := range chips {
		tensor, err := makeTensorFromImage(fmt.Sprintf("/tmp/chip-%v.png", i))
		if err != nil {
			log.Fatal(err)
		}
		output, err := session.Run(
			map[tf.Output]*tf.Tensor{
				graph.Operation("image_tensor").Output(0): tensor,
			},
			[]tf.Output{
				graph.Operation("detection_boxes").Output(0),
				graph.Operation("detection_scores").Output(0),
				graph.Operation("detection_classes").Output(0),
				graph.Operation("num_detections").Output(0),
			},
			nil)
		if err != nil {
			log.Fatal(err)
		}

		boxes := output[0].Value().([][][]float32)[0]
		scores := output[1].Value().([][]float32)[0]
		classes := output[2].Value().([][]float32)[0]

		detects := make([]Detect, 1)
		for i, score := range scores {
			class := classes[i]
			detects = append(detects,
				Detect{
					Bounds:      transformBox(chip.X, chip.Y, boxes[i]),
					Class:       int(class),
					Chip:        &chip,
					Description: "",
					Confidence:  score,
				})
		}
		printDetections(detects, labelsfile)
	}
}

func transformBox(chipX, chipY int, box []float32) image.Rectangle {
	//     chip pos   ->  world pos
	mx := int(box[0]*W) + (chipX * W)
	Mx := int(box[2]*W) + (chipX * W)
	my := int(box[1]*H) + (chipY * H)
	My := int(box[3]*H) + (chipY * H)

	return image.Rectangle{
		Min: image.Point{
			X: mx,
			Y: my,
		},
		Max: image.Point{
			X: Mx,
			Y: My,
		},
	}
}

func printDetections(detects []Detect, labelsFile string) {
	file, err := os.Open(labelsFile)
	if err != nil {
		log.Fatal(err)
	}
	defer file.Close()
	scanner := bufio.NewScanner(file)
	if err := scanner.Err(); err != nil {
		log.Printf("ERROR: failed to read %s: %v", labelsFile, err)
	}

	labels := make(map[int]string)
	for scanner.Scan() {
		splits := strings.Split(scanner.Text(), ":")
		id, _ := strconv.Atoi(splits[0])
		labels[id] = splits[1]
	}

	for _, d := range detects {
		fmt.Printf("%v %v %v %v %v %v\n", d.Bounds.Min.X, d.Bounds.Min.Y, d.Bounds.Max.X, d.Bounds.Max.Y, d.Class, d.Confidence)
	}
}

// Convert the image in filename to a Tensor suitable as input to the Inception model.
func makeTensorFromImage(filename string) (*tf.Tensor, error) {
	bytes, err := ioutil.ReadFile(filename)
	if err != nil {
		return nil, err
	}
	// DecodeJpeg uses a scalar String-valued tensor as input.
	tensor, err := tf.NewTensor(string(bytes))
	if err != nil {
		return nil, err
	}
	// Construct a graph to normalize the image
	graph, input, output, err := constructGraphToNormalizeImage()
	if err != nil {
		return nil, err
	}
	// Execute that graph to normalize this one image
	session, err := tf.NewSession(graph, nil)
	if err != nil {
		return nil, err
	}
	defer session.Close()
	normalized, err := session.Run(
		map[tf.Output]*tf.Tensor{input: tensor},
		[]tf.Output{output},
		nil)
	if err != nil {
		return nil, err
	}
	return normalized[0], nil
}

func constructGraphToNormalizeImage() (graph *tf.Graph, input, output tf.Output, err error) {
	s := op.NewScope()
	input = op.Placeholder(s, tf.String)
	output = op.ExpandDims(s,
		op.DecodeJpeg(s, input, op.DecodeJpegChannels(3)),
		op.Const(s.SubScope("make_batch"), int32(0)))

	graph, err = s.Finalize()
	return graph, input, output, err
}

func modelFiles(dir string, name string) (m string, l string, e error) {
	return filepath.Join(dir, fmt.Sprintf("%v.pb", name)), filepath.Join(dir, "labels.txt"), nil
}

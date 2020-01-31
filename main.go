package main

import (
	"bufio"
	"flag"
	"fmt"
	tf "github.com/tensorflow/tensorflow/tensorflow/go"
	"github.com/tensorflow/tensorflow/tensorflow/go/op"
	"io/ioutil"
	"log"
	"os"
	"path/filepath"
)

func main() {
	// An example for using the TensorFlow Go API for image recognition
	// using a pre-trained inception model (http://arxiv.org/abs/1512.00567).
	//
	// Sample usage: <program> -dir=/tmp/modeldir -image=/path/to/some/jpeg
	//
	// The pre-trained model takes input in the form of a 4-dimensional
	// tensor with shape [ BATCH_SIZE, IMAGE_HEIGHT, IMAGE_WIDTH, 3 ],
	// where:
	// - BATCH_SIZE allows for inference of multiple images in one pass through the graph
	// - IMAGE_HEIGHT is the height of the images on which the model was trained
	// - IMAGE_WIDTH is the width of the images on which the model was trained
	// - 3 is the (R, G, B) values of the pixel colors represented as a float.
	//
	// And produces as output a vector with shape [ NUM_LABELS ].
	// output[i] is the probability that the input image was recognized as
	// having the i-th label.
	//
	// A separate file contains a list of string labels corresponding to the
	// integer indices of the output.
	//
	// This example:
	// - Loads the serialized representation of the pre-trained model into a Graph
	// - Creates a Session to execute operations on the Graph
	// - Converts an image file to a Tensor to provide as input to a Session run
	// - Executes the Session and prints out the label with the highest probability
	//
	// To convert an image file to a Tensor suitable for input to the Inception model,
	// this example:
	// - Constructs another TensorFlow graph to normalize the image into a
	//   form suitable for the model (for example, resizing the image)
	// - Creates and executes a Session to obtain a Tensor in this normalized form.
	modeldir := flag.String("dir", "", "Directory containing the trained model and labels")
	imagefile := flag.String("image", "", "Path of a JPEG-image to extract labels for")
	flag.Parse()
	if *modeldir == "" || *imagefile == "" {
		flag.Usage()
		return
	}
	// Load the serialized GraphDef from a file.
	modelfile, labelsfile, err := modelFiles(*modeldir, "vanilla")
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

	// Run inference on *imageFile.
	// For multiple images, session.Run() can be called in a loop (and
	// concurrently). Alternatively, images can be batched since the model
	// accepts batches of image data as input.
	tensor, err := makeTensorFromImage(*imagefile)
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
	// output[0].Value() is a vector containing probabilities of
	// labels for each image in the "batch". The batch size was 1.
	// Find the most probably label index.
	probabilities := output[1].Value().([][]float32)[0]
	printBestLabel(probabilities, labelsfile)
}

func printBestLabel(probabilities []float32, labelsFile string) {
	bestIdx := 0
	for i, p := range probabilities {
		if p > probabilities[bestIdx] {
			bestIdx = i
		}
	}
	// Found the best match. Read the string from labelsFile, which
	// contains one line per label.
	file, err := os.Open(labelsFile)
	if err != nil {
		log.Fatal(err)
	}
	defer file.Close()
	scanner := bufio.NewScanner(file)
	var labels []string
	for scanner.Scan() {
		labels = append(labels, scanner.Text())
	}
	if err := scanner.Err(); err != nil {
		log.Printf("ERROR: failed to read %s: %v", labelsFile, err)
	}
	fmt.Printf("BEST MATCH: (%2.0f%% likely) %s\n", probabilities[bestIdx]*100.0, labels[bestIdx])
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

// The inception model takes as input the image described by a Tensor in a very
// specific normalized format (a particular image size, shape of the input tensor,
// normalized pixel values etc.).
//
// This function constructs a graph of TensorFlow operations which takes as
// input a JPEG-encoded string and returns a tensor suitable as input to the
// inception model.
func constructGraphToNormalizeImage() (graph *tf.Graph, input, output tf.Output, err error) {
	// Some constants specific to the pre-trained model at:
	// https://storage.googleapis.com/download.tensorflow.org/models/inception5h.zip
	//
	// - The model was trained after with images scaled to 224x224 pixels.
	// - The colors, represented as R, G, B in 1-byte each were converted to
	//   float using (value - Mean)/Scale.
	const (
		H, W  = 224, 224
		Mean  = float32(117)
		Scale = float32(1)
	)
	// - input is a String-Tensor, where the string the JPEG-encoded image.
	// - The inception model takes a 4D tensor of shape
	//   [BatchSize, Height, Width, Colors=3], where each pixel is
	//   represented as a triplet of floats
	// - Apply normalization on each pixel and use ExpandDims to make
	//   this single image be a "batch" of size 1 for ResizeBilinear.
	s := op.NewScope()
	input = op.Placeholder(s, tf.String)
	output = op.Div(s,
		op.Sub(s,
			op.ResizeBilinear(s,
				op.ExpandDims(s,
					op.Cast(s, op.DecodeJpeg(s, input, op.DecodeJpegChannels(3)), tf.Float),
					op.Const(s.SubScope("make_batch"), int32(0))),
				op.Const(s.SubScope("size"), []int32{H, W})),
			op.Const(s.SubScope("mean"), Mean)),
		op.Const(s.SubScope("scale"), Scale))

	// https://github.com/tensorflow/models/issues/1741#issuecomment-317501641
	output = op.Cast(s.SubScope("final_resize"), output, tf.Uint8)

	graph, err = s.Finalize()
	return graph, input, output, err
}

func modelFiles(dir string, name string) (m string, l string, e error) {
	return filepath.Join(dir, fmt.Sprintf("%v.pb", name)), filepath.Join(dir, "labels.txt"), nil
}

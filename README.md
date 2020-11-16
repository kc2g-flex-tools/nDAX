## Table of Contents

* [About the Project](#about-the-project)
  * [Built With](#built-with)
* [Getting Started](#getting-started)
  * [Prerequisites](#prerequisites)
  * [Installation](#installation)
* [Usage](#usage)
* [Roadmap](#roadmap)
* [Contributing](#contributing)
* [License](#license)
* [Contact](#contact)
* [Acknowledgements](#acknowledgements)



<!-- ABOUT THE PROJECT -->
## About The Project

We absolutely love our Flex radios! We just wish we had a Linux-native way to work with them so that Windows or Maestro isn't required.

nDAX creates a PulseAudio device for your slice, and then connects up the audio for RX and TX.

### Built With

* [Go](https://golang.org)

## Getting Started

```
$ ./nDAX -station station_name -slice A -daxch 1 -sink flex.sliceA.rx -source flex.sliceA.tx
```

### Installation

1. Download the latest binary using the Releases icon above or [Releases](https://github.com/kc2g-flex-tools/nDAX/releases)
2. Make it executible: `chmod +x nDAX`

<!-- USAGE EXAMPLES -->
## Usage

```
$ ./nDAX -h
Usage of ./nDAX:
  -daxch string
        DAX channel # to use (default "1")
  -latency float
        Target RX latency (ms, higher = less sample rate variation) (default 100)
  -radio string
        radio IP address or discovery spec (default ":discover:")
  -sink string
        PulseAudio sink to send audio to (default "flexdax.rx")
  -slice string
        Slice letter to use (default "A")
  -source string
        PulseAudio sink to receive from (default "flexdax.tx")
  -station string
        station name to bind to (default "Flex")
```
### Multiple Instances

To run multiple nDAX instances, see the scripts directory.

### Audio Levels

The best way to adjust nDAX receive audio levels is with the Flex's "AGC-T" control. If you are running nCAT, you can
adjust this using `rigctl`. The `.30` value should be between 0 and 1, and the port needs to correspond to your port number from nCAT:

```
$ rigctl -m 2 -r localhost:4532 L RF .30
```

You can also adjust receive and transmit audio levels on a per-slice basis using a mixer app like `pavucontrol` or the KDE or GNOME mixers.

### Using with WSJT-X

To setup WSJT-X with nCAT and hamlib, use the following settings:

![WSJT-X Setup](https://raw.githubusercontent.com/kc2g-flex-tools/nDAX/master/docs/wsjtx_audio.png)

<!-- ROADMAP -->
## Roadmap

See the [open issues](https://github.com/kc2g-flex-tools/nDAX/issues) for a list of proposed features (and known issues).

<!-- CONTRIBUTING -->
## Contributing

Contributions are what make the open source community such an amazing place to be learn, inspire, and create. Any contributions you make are **greatly appreciated**.

1. Fork the Project
2. Create your Feature Branch (`git checkout -b feature/AmazingFeature`)
3. Commit your Changes (`git commit -m 'Add some AmazingFeature'`)
4. Push to the Branch (`git push origin feature/AmazingFeature`)
5. Open a Pull Request


<!-- LICENSE -->
## License

MIT License. See LICENSE file.


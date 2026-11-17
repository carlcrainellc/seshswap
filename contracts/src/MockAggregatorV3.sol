// SPDX-License-Identifier: MIT
pragma solidity ^0.8.26;

/// @title MockAggregatorV3
/// @notice Test-only AggregatorV3Interface stand-in. Used on chains that have
///         no official Chainlink XMR/USD feed (Arbitrum Sepolia has none).
///         8 decimals, same as Chainlink USD feeds. The owner can update the answer.
contract MockAggregatorV3 {
    uint8 public constant decimals = 8;
    string public description;
    uint256 public version = 1;

    int256 private _answer;
    uint80 private _round;
    uint256 private _updatedAt;
    address public owner;

    error NotOwner();

    constructor(string memory desc, int256 initialAnswer) {
        owner = msg.sender;
        description = desc;
        _answer = initialAnswer;
        _round = 1;
        _updatedAt = block.timestamp;
    }

    function updateAnswer(int256 answer) external {
        if (msg.sender != owner) revert NotOwner();
        _answer = answer;
        unchecked {
            _round += 1;
        }
        _updatedAt = block.timestamp;
    }

    function latestRoundData()
        external
        view
        returns (uint80 roundId, int256 answer, uint256 startedAt, uint256 updatedAt, uint80 answeredInRound)
    {
        return (_round, _answer, _updatedAt, _updatedAt, _round);
    }

    function getRoundData(uint80)
        external
        view
        returns (uint80 roundId, int256 answer, uint256 startedAt, uint256 updatedAt, uint80 answeredInRound)
    {
        return (_round, _answer, _updatedAt, _updatedAt, _round);
    }
}
